// Package identity recognises the feedback service's own writers.
//
// Milo's authorization decides which verbs a caller may use on a resource.
// Some rules are about fields within a resource (only feedback-sync writes a
// post's GitHub fields, only feedback-controller writes its delivery
// tracker), which IAM can't express, so strategies check the caller here.
package identity

import (
	"context"
	"slices"
	"strings"

	"k8s.io/apiserver/pkg/endpoints/request"
)

// Identities holds the usernames of the service's own components.
type Identities struct {
	// Sync are feedback-sync's usernames. It is the only writer of
	// GitHub-derived content.
	Sync []string
	// Controller are feedback-controller's usernames. It writes triage and
	// delivery tracking.
	Controller []string
}

// Default usernames, matching the Kubernetes service accounts in
// feedback-system. Override with flags where Milo names them differently.
var Default = Identities{
	Sync:       []string{"system:serviceaccount:feedback-system:feedback-sync"},
	Controller: []string{"system:serviceaccount:feedback-system:feedback-controller"},
}

// Caller is who a request comes from, as far as field ownership goes.
type Caller int

const (
	// Other is anyone else: staff, the website, users.
	Other Caller = iota
	Sync
	Controller
)

func (c Caller) String() string {
	switch c {
	case Sync:
		return "feedback-sync"
	case Controller:
		return "feedback-controller"
	default:
		return "other"
	}
}

// Of returns the caller for ctx.
func (ids Identities) Of(ctx context.Context) Caller {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return Other
	}
	switch name := u.GetName(); {
	case slices.Contains(ids.Sync, name):
		return Sync
	case slices.Contains(ids.Controller, name):
		return Controller
	default:
		return Other
	}
}

// EmailVerifiedExtraKey is set by Milo's identity provider on every human
// identity, "true" or "false". Machine identities don't carry it.
const EmailVerifiedExtraKey = "iam.miloapis.com/emailVerified"

// EmailUnverified reports whether the caller is a person whose email address
// isn't verified. Machine identities carry no verdict and pass, as in Milo's
// own admission check. Extras reach an aggregated API as request headers,
// which lower-cases their keys, so the key is matched case-insensitively.
func EmailUnverified(ctx context.Context) bool {
	u, ok := request.UserFrom(ctx)
	if !ok {
		return false
	}
	for k, vals := range u.GetExtra() {
		if strings.EqualFold(k, EmailVerifiedExtraKey) && !(len(vals) == 1 && vals[0] == "true") {
			return true
		}
	}
	return false
}
