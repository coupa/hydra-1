package warden

import (
	"context"
	"net/http"
	"time"

	"github.com/coupa/foundation-go/metrics"
	"github.com/ory/fosite"
	"github.com/ory/hydra/firewall"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/hydra/pkg"
	"github.com/ory/hydra/warden/group"
	"github.com/ory/ladon"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type LocalWarden struct {
	Warden ladon.Warden
	OAuth2 fosite.OAuth2Provider
	Groups group.Manager

	AccessTokenLifespan time.Duration
	Issuer              string
	L                   logrus.FieldLogger
}

func (w *LocalWarden) TokenFromRequest(r *http.Request) string {
	return fosite.AccessTokenFromRequest(r)
}

func (w *LocalWarden) IsAllowed(ctx context.Context, a *firewall.AccessRequest) error {
	statsdResource := pkg.SanitizeForStatsd(a.Resource, "_")

	if err := w.isAllowed(ctx, &ladon.Request{
		Resource: a.Resource,
		Action:   a.Action,
		Subject:  a.Subject,
		Context:  a.Context,
	}); err != nil {
		w.L.WithFields(logrus.Fields{
			"subject": a.Subject,
			"request": a,
			"reason":  "The policy decision point denied the request",
		}).WithError(err).Infof("Access denied")

		metrics.Increment("Warden.IsAllowed.Failure", map[string]string{
			"client_id": a.Subject,
			"resource":  statsdResource,
			"reason":    "The policy decision point denied the request",
		})
		return err
	}

	w.L.WithFields(logrus.Fields{
		"subject": a.Subject,
		"request": a,
		"reason":  "The policy decision point allowed the request",
	}).Infof("Access allowed")

	metrics.Increment("Warden.IsAllowed.Success", map[string]string{
		"client_id": a.Subject,
		"resource":  statsdResource,
	})
	return nil
}

func (w *LocalWarden) TokenAllowed(ctx context.Context, token string, a *firewall.TokenAccessRequest, scopes ...string) (*firewall.Context, error) {
	c := w.newBasicContext()
	statsdResource := pkg.SanitizeForStatsd(a.Resource, "_")
	statsdAction := pkg.SanitizeForStatsd(a.Action, "_")

	var auth, err = w.OAuth2.IntrospectToken(ctx, token, fosite.AccessToken, oauth2.NewSession(""), scopes...)
	if err != nil {
		w.L.WithFields(logrus.Fields{
			"request": a,
			"reason":  "Token is expired, malformed or missing",
		}).WithError(err).Infof("Access denied")

		metrics.Increment("Warden.TokenAllowed.Failure", map[string]string{
			"client_id": "",
			"resource":  statsdResource,
			"reason":    "Token is expired, malformed or missing",
			"action":    statsdAction,
		})
		return c, err
	}

	session := auth.GetSession()
	if err := w.isAllowed(ctx, &ladon.Request{
		Resource: a.Resource,
		Action:   a.Action,
		Subject:  session.GetSubject(),
		Context:  a.Context,
	}); err != nil {
		w.L.WithFields(logrus.Fields{
			"scopes":   scopes,
			"subject":  session.GetSubject(),
			"audience": auth.GetClient().GetID(),
			"request":  a,
			"reason":   "The policy decision point denied the request",
		}).WithError(err).Infof("Access denied")

		metrics.Increment("Warden.TokenAllowed.Failure", map[string]string{
			"client_id": session.GetSubject(),
			"resource":  statsdResource,
			"reason":    "The policy decision point denied the request",
			"action":    statsdAction,
		})
		c.Subject = session.GetSubject()
		return c, err
	}

	c = w.newContext(auth)
	w.L.WithFields(logrus.Fields{
		"subject":  c.Subject,
		"audience": auth.GetClient().GetID(),
		"request":  auth,
		"result":   c,
	}).Infof("Access granted")

	metrics.Increment("Warden.TokenAllowed.Success", map[string]string{
		"client_id": c.Subject,
		"resource":  statsdResource,
		"action":    statsdAction,
	})
	return c, nil
}

func (w *LocalWarden) isAllowed(ctx context.Context, a *ladon.Request) error {
	groups, err := w.Groups.FindGroupNames(a.Subject)
	if err != nil {
		return err
	}

	errs := make([]error, len(groups)+1)
	errs[0] = w.Warden.IsAllowed(&ladon.Request{
		Resource: a.Resource,
		Action:   a.Action,
		Subject:  a.Subject,
		Context:  a.Context,
	})

	for k, g := range groups {
		errs[k+1] = w.Warden.IsAllowed(&ladon.Request{
			Resource: a.Resource,
			Action:   a.Action,
			Subject:  g,
			Context:  a.Context,
		})
	}

	for _, err := range errs {
		if errors.Cause(err) == ladon.ErrRequestForcefullyDenied {
			return errors.Wrap(fosite.ErrRequestForbidden, err.Error())
		}
	}

	for _, err := range errs {
		if err == nil {
			return nil
		}
	}

	return errors.Wrap(fosite.ErrRequestForbidden, ladon.ErrRequestDenied.Error())
}

func (w *LocalWarden) newContext(auth fosite.AccessRequester) *firewall.Context {
	session := auth.GetSession().(*oauth2.Session)

	exp := auth.GetSession().GetExpiresAt(fosite.AccessToken)
	if exp.IsZero() {
		exp = auth.GetRequestedAt().Add(w.AccessTokenLifespan)
	}

	c := &firewall.Context{
		Subject:       session.Subject,
		GrantedScopes: auth.GetGrantedScopes(),
		Issuer:        w.Issuer,
		Audience:      auth.GetClient().GetID(),
		IssuedAt:      auth.GetRequestedAt(),
		ExpiresAt:     exp,
		Extra:         session.Extra,
	}

	return c
}

func (w *LocalWarden) newBasicContext() *firewall.Context {
	return &firewall.Context{}
}
