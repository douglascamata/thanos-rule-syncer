package v1

import (
	"context"
	"errors"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/go-chi/chi"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/rulefmt"
	"gopkg.in/yaml.v3"

	"github.com/observatorium/api/authentication"
)

// ErrRuleNotFound is returned when a particular rule wasn't found by its name.
var ErrRuleNotFound = errors.New("rule not found")

type RuleGroups struct {
	Groups []RuleGroup `yaml:"groups"`
}

type RuleGroup struct {
	Name     string         `yaml:"name"`
	Interval model.Duration `yaml:"interval"`
	Rules    []rulefmt.Rule `yaml:"rules"`
}

// RulesRepository describes all of the operations that a conformant repository should implement.
type RulesRepository interface {
	RulesLister
	RulesGetter
	RulesWriter
}

type RulesLister interface {
	ListRuleGroups(ctx context.Context, tenant string) (RuleGroups, error)
}

func listRulesHandler(logger log.Logger, lister RulesLister, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := authentication.GetTenant(r.Context())
		if !ok {
			http.Error(w, "failed to get tenant", http.StatusInternalServerError)
			return
		}

		id, ok := authentication.GetTenantID(r.Context())
		if !ok {
			const msg = "error finding tenant ID"
			level.Warn(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		rules, err := lister.ListRuleGroups(r.Context(), tenant)
		if err != nil {
			msg := "failed to list rules"
			level.Debug(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		for i := range rules.Groups {
			for j := range rules.Groups[i].Rules {
				if rules.Groups[i].Rules[j].Labels == nil {
					rules.Groups[i].Rules[j].Labels = make(map[string]string)
				}
				rules.Groups[i].Rules[j].Labels[label] = id
			}
		}

		bytes, err := yaml.Marshal(rules)
		if err != nil {
			msg := "failed to marshal rules"
			level.Debug(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		_, _ = w.Write(bytes)
	}
}

type RulesGetter interface {
	GetRules(ctx context.Context, tenant, name string) (RuleGroup, error)
}

func getRuleHandler(logger log.Logger, repository RulesGetter, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := authentication.GetTenant(r.Context())
		if !ok {
			http.Error(w, "failed to get tenant", http.StatusInternalServerError)
			return
		}

		id, ok := authentication.GetTenantID(r.Context())
		if !ok {
			const msg = "error finding tenant ID"
			level.Warn(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		name := chi.URLParam(r, "name")

		rules, err := repository.GetRules(r.Context(), tenant, name)
		if err == ErrRuleNotFound {
			msg := "rule not found"
			level.Debug(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		if err != nil {
			msg := "failed to get rules"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		for i := range rules.Rules {
			if rules.Rules[i].Labels == nil {
				rules.Rules[i].Labels = make(map[string]string)
			}
			rules.Rules[i].Labels[label] = id
		}

		bytes, err := yaml.Marshal(rules)
		if err != nil {
			msg := "failed to marshal rules"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		_, _ = w.Write(bytes)
	}
}

const editHTML = `
<html lang="en">
<head>
    <title>Edit Rules - Observatorium</title>
</head>
<body>
    <h3>Edit Rule {{ .Name }}</h3>
    <form action="/api/metrics/v1/{{ .Tenant }}/rules/{{ .Name }}" method="post">
        <textarea cols="120" rows="30" name="rulegroup">{{ .Rules }}</textarea><br>
        <button type="submit">Update</button>
    </form>
</body>
</html>
`

func editRuleHandler(logger log.Logger, repository RulesGetter) http.HandlerFunc {
	tmpl, err := template.New("edit").Parse(editHTML)
	if err != nil {
		level.Error(logger).Log("msg", "failed to parse rule edit HTML", "err", err)
		os.Exit(1)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := authentication.GetTenant(r.Context())
		if !ok {
			http.Error(w, "failed to get tenant", http.StatusInternalServerError)
			return
		}
		name := chi.URLParam(r, "name")

		rules, err := repository.GetRules(r.Context(), tenant, name)
		if err == ErrRuleNotFound {
			const msg = "rule not found"
			level.Debug(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		if err != nil {
			const msg = "failed to get rules"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		bytes, err := yaml.Marshal(rules)
		if err != nil {
			const msg = "failed to marshal rules"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		_ = tmpl.Execute(w, struct {
			Name   string
			Rules  string
			Tenant string
		}{
			Name:   name,
			Rules:  string(bytes),
			Tenant: tenant,
		})
	}
}

type RulesWriter interface {
	CreateRule(ctx context.Context, tenant, name string, interval int64, rules []byte) error
	UpdateRule(ctx context.Context, tenant, name string, interval int64, rules []byte) error
}

func writeRuleHandler(logger log.Logger, repository RulesWriter, label string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := authentication.GetTenant(r.Context())
		if !ok {
			const msg = "failed to get tenant"
			level.Warn(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		id, ok := authentication.GetTenantID(r.Context())
		if !ok {
			const msg = "error finding tenant ID"
			level.Warn(logger).Log("msg", msg)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		name := chi.URLParam(r, "name")

		defer r.Body.Close()

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			const msg = "failed to read rules from request body"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		var group RuleGroup
		if err := yaml.Unmarshal(body, &group); err != nil {
			const msg = "failed to unmarshal YAML to rule group"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		for i := range group.Rules {
			if group.Rules[i].Labels == nil {
				group.Rules[i].Labels = make(map[string]string)
			}
			group.Rules[i].Labels[label] = id
		}

		rules, err := yaml.Marshal(group.Rules)
		if err != nil {
			const msg = "failed to unmarshal YAML to rule group"
			level.Warn(logger).Log("msg", msg, "err", err)
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		switch r.Method {
		case http.MethodPost:
			if err := repository.CreateRule(r.Context(), tenant, name, int64(group.Interval), rules); err != nil {
				const msg = "failed to create rules"
				level.Warn(logger).Log("msg", msg, "err", err)
				http.Error(w, msg, http.StatusInternalServerError)
				return
			}
		case http.MethodPut:
			if err := repository.UpdateRule(r.Context(), tenant, name, int64(group.Interval), rules); err != nil {
				const msg = "failed to update rules"
				level.Warn(logger).Log("msg", msg, "err", err)
				http.Error(w, msg, http.StatusInternalServerError)
				return
			}
		}
	}
}
