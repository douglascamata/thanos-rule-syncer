package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/metalmatze/signal/internalserver"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

type config struct {
	rulesBackendURL  string
	observatoriumURL string
	observatoriumCA  string
	thanosRuleURL    string
	file             string
	tenant           string
	oidc             oidcConfig
	interval         uint

	listenInternal string
}

type oidcConfig struct {
	audience     string
	clientID     string
	clientSecret string
	issuerURL    string
}

func parseFlags() *config {
	cfg := &config{}

	// Common flags.
	flag.StringVar(&cfg.file, "file", "rules.yaml", "The path to the file the rules are written to on disk so that Thanos Ruler can read it from. Required.")
	flag.StringVar(&cfg.thanosRuleURL, "thanos-rule-url", "", "The URL of Thanos Ruler that is used to trigger reloads of rules. We will append /-/reload. Required.")
	flag.UintVar(&cfg.interval, "interval", 60, "The interval at which to poll the Observatorium API for updates to rules, given in seconds.")

	// Use rules backend where no auth is needed and only single instance of thanos-rule-syncer sidecar is required.
	flag.StringVar(&cfg.rulesBackendURL, "rules-backend-url", "", "The URL of the Rules Storage Backend from which to fetch the rules. If specified, it gets priority over -observatorium-api-url and auth flags are no longer needed.")

	// Use Observatorium API, which requires auth and needs a thanos-rule-syncer sidecar per tenant.
	flag.StringVar(&cfg.observatoriumURL, "observatorium-api-url", "", "The URL of the Observatorium API from which to fetch the rules. If specified, auth flags must also be provided.")
	flag.StringVar(&cfg.tenant, "tenant", "", "The name of the tenant whose rules should be synced.")
	flag.StringVar(&cfg.observatoriumCA, "observatorium-ca", "", "Path to a file containing the TLS CA against which to verify the Observatorium API. If no server CA is specified, the client will use the system certificates.")
	flag.StringVar(&cfg.oidc.issuerURL, "oidc.issuer-url", "", "The OIDC issuer URL, see https://openid.net/specs/openid-connect-discovery-1_0.html#IssuerDiscovery.")
	flag.StringVar(&cfg.oidc.clientSecret, "oidc.client-secret", "", "The OIDC client secret, see https://tools.ietf.org/html/rfc6749#section-2.3.")
	flag.StringVar(&cfg.oidc.clientID, "oidc.client-id", "", "The OIDC client ID, see https://tools.ietf.org/html/rfc6749#section-2.3.")
	flag.StringVar(&cfg.oidc.audience, "oidc.audience", "", "The audience for whom the access token is intended, see https://openid.net/specs/openid-connect-core-1_0.html#IDToken.")

	flag.StringVar(&cfg.listenInternal, "web.internal.listen", ":8083", "The address on which the internal server listens.")

	flag.Parse()
	return cfg
}

func main() {
	cfg := parseFlags()

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		//nolint:exhaustivestruct
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	roundTripperInst := newRoundTripperInstrumenter(registry)

	ctx, cancel := context.WithCancel(context.Background())
	t := http.DefaultTransport.(*http.Transport).Clone()

	if cfg.observatoriumCA != "" {
		caFile, err := os.ReadFile(cfg.observatoriumCA)
		if err != nil {
			log.Fatalf("failed to read Observatorium CA file: %v", err)
		}

		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(caFile)
		t.TLSClientConfig = &tls.Config{
			RootCAs: certPool,
		}
	}

	clientFetcher := &http.Client{
		Transport: roundTripperInst.NewRoundTripper("fetch", t),
	}
	clientReloader := &http.Client{
		Transport: roundTripperInst.NewRoundTripper("reload", t),
	}

	if cfg.oidc.issuerURL != "" {
		provider, err := oidc.NewProvider(context.Background(), cfg.oidc.issuerURL)
		if err != nil {
			log.Fatalf("OIDC provider initialization failed: %v", err)
		}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, http.Client{
			Transport: roundTripperInst.NewRoundTripper("oauth", http.DefaultTransport),
		})
		ccc := clientcredentials.Config{
			ClientID:     cfg.oidc.clientID,
			ClientSecret: cfg.oidc.clientSecret,
			TokenURL:     provider.Endpoint().TokenURL,
		}
		if cfg.oidc.audience != "" {
			ccc.EndpointParams = url.Values{
				"audience": []string{cfg.oidc.audience},
			}
		}
		clientFetcher = &http.Client{
			Transport: &oauth2.Transport{
				Base:   clientFetcher.Transport,
				Source: ccc.TokenSource(ctx),
			},
		}
	}

	var f fetcher

	if cfg.rulesBackendURL != "" {
		rulesFetcher, err := newRulesBackendFetcher(cfg.rulesBackendURL, clientFetcher)
		if err != nil {
			log.Fatalf("failed to initialize Rules Backend fetcher: %v", err)
		}
		f = rulesFetcher
	} else {
		obsFetcher, err := newObservatoriumAPIFetcher(cfg.observatoriumURL, cfg.tenant, clientFetcher)
		if err != nil {
			log.Fatalf("failed to initialize Observatorium API fetcher: %v", err)
		}
		f = obsFetcher
	}

	var gr run.Group
	gr.Add(run.SignalHandler(ctx, os.Interrupt))

	gr.Add(func() error {
		fn := func(ctx context.Context) error {
			rules, err := f.getRules(ctx)
			if err != nil {
				return fmt.Errorf("failed to get rules from url: %v", err)
			}
			defer rules.Close()
			file, err := os.Create(cfg.file)
			if err != nil {
				return fmt.Errorf("failed to create or open the rules file %s: %v", cfg.file, err)
			}
			w := bufio.NewWriter(file)
			if _, err = w.ReadFrom(rules); err != nil {
				return fmt.Errorf("failed to write to rules file %s: %v", cfg.file, err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("failed to close the rules file %s: %v", cfg.file, err)
			}
			if err := reloadThanosRule(ctx, clientReloader, cfg.thanosRuleURL); err != nil {
				return fmt.Errorf("failed to trigger thanos rule reload: %v", err)
			}
			return nil
		}
		if err := fn(ctx); err != nil {
			log.Print(err.Error())
		}
		ticker := time.NewTicker(time.Duration(cfg.interval) * time.Second)
		for {
			select {
			case <-ticker.C:
				if err := fn(ctx); err != nil {
					log.Print(err.Error())
				}
			case <-ctx.Done():
				return nil
			}
		}
	}, func(err error) {
		cancel()
	})

	{
		h := internalserver.NewHandler(
			internalserver.WithName("Internal - thanos-rule-syncer"),
			internalserver.WithPrometheusRegistry(registry),
			internalserver.WithPProf(),
		)

		//nolint:exhaustivestruct
		s := http.Server{
			Addr:    cfg.listenInternal,
			Handler: h,
		}

		gr.Add(func() error {
			log.Print("starting internal HTTP server at address: ", s.Addr)

			return s.ListenAndServe() //nolint:wrapcheck
		}, func(_ error) {
			_ = s.Shutdown(context.Background())
		})
	}

	if err := gr.Run(); err != nil {
		log.Fatalf("thanos-rule-syncer quit unexpectectly: %v", err)
	}
}

func reloadThanosRule(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/-/reload", url), nil)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("got unexpected status from Thanos Ruler: %d", res.StatusCode)
	}

	return nil
}
