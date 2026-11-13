package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mbevc1/mrsh/internal/config"
)

// placeholderARN stands in for a parameter's ARN while the edit is checked,
// before anything is stored.
const placeholderARN = "arn:aws:ssm:us-east-1:000000000000:parameter"

// storeSSM writes every kind "ssm" secret to SSM and turns it into kind
// "arn" with the parameter's ARN. check runs first, with placeholder ARNs,
// so an edit the config would refuse stores nothing. owner and name pick
// the default parameter name.
func (s *Server) storeSSM(ctx context.Context, cfg *config.Config, owner, name string, check func() error, secrets ...*Secret) error {
	type pending struct {
		sec         *Secret
		field, name string
		value       string
		overwrite   bool
	}
	var todo []pending
	for i, sec := range secrets {
		if sec.Kind != "ssm" {
			continue
		}
		field := []string{"user", "pass"}[i]
		param := strings.TrimSpace(sec.Name)
		if param == "" {
			if owner != config.OwnerDefaults && name == "" {
				return &config.ValidationError{Err: fmt.Errorf("%s: a name is needed for the default SSM parameter", field)}
			}
			param = cfg.SSMName(owner, name, field)
		}
		if err := config.ValidSSMName(param); err != nil {
			return &config.ValidationError{Err: fmt.Errorf("%s: SSM parameter %w", field, err)}
		}
		if sec.Value == "" {
			return &config.ValidationError{Err: fmt.Errorf("%s: enter the value to store in SSM", field)}
		}
		todo = append(todo, pending{sec: sec, field: field, name: param, value: sec.Value, overwrite: sec.Overwrite})
		*sec = Secret{Kind: "arn", Value: placeholderARN + param}
	}
	if len(todo) == 0 {
		return nil
	}
	if s.Secrets == nil {
		return &config.ValidationError{Err: errors.New("storing secrets in SSM is not available")}
	}
	if err := check(); err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) || errors.Is(err, config.ErrHostExists) {
			return err
		}
		return &config.ValidationError{Err: err}
	}
	for _, p := range todo {
		arn, err := s.Secrets.PutSecret(ctx, p.name, p.value, p.overwrite)
		if errors.Is(err, config.ErrSecretExists) {
			return &config.ValidationError{Err: fmt.Errorf("%s: SSM parameter %s already exists; tick \"Replace an existing parameter\" to overwrite it", p.field, p.name)}
		}
		if err != nil {
			return &config.ValidationError{Err: fmt.Errorf("%s: %w", p.field, err)}
		}
		*p.sec = Secret{Kind: "arn", Value: arn}
	}
	return nil
}

// afterSave holds work an edit asks to run once the config is saved; each
// function's map is merged into the reply.
type afterSave struct {
	fns []func(context.Context) map[string]any
}

type afterSaveKey struct{}

// onSaved queues fn to run after the edit in r is saved.
func onSaved(r *http.Request, fn func(context.Context) map[string]any) {
	if a, ok := r.Context().Value(afterSaveKey{}).(*afterSave); ok {
		a.fns = append(a.fns, fn)
	}
}

// SecretsReport is the "secrets" part of a removal's reply.
type SecretsReport struct {
	Deleted []string `json:"deleted"`
	Kept    []string `json:"kept"`
	Errors  []string `json:"errors"`
}

// deleteSecretsAfterSave deletes the SSM parameters a removal planned
// (config.SecretDeletes or HostSecretDeletes) once the removal is saved,
// never before.
func (s *Server) deleteSecretsAfterSave(r *http.Request, cfg *config.Config, del, kept []string) error {
	if len(del) > 0 && s.Secrets == nil {
		return &config.ValidationError{Err: errors.New("deleting SSM parameters is not available")}
	}
	onSaved(r, func(ctx context.Context) map[string]any {
		rep := SecretsReport{Deleted: []string{}, Kept: append([]string{}, kept...), Errors: []string{}}
		for _, arn := range del {
			name, _ := cfg.ManagedSSMName(arn)
			if err := s.Secrets.DeleteSecret(ctx, arn); err != nil {
				rep.Errors = append(rep.Errors, err.Error())
				continue
			}
			rep.Deleted = append(rep.Deleted, name)
		}
		return map[string]any{"secrets": rep}
	})
	return nil
}
