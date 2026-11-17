// Package identity reads who is making a request, from the user info Milo
// passes to admission webhooks.
package identity

import (
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// Usernames of the service's own components in Milo: the common names of
// the client certificates they present.
const (
	OperatorUser = "system:feedback-operator@feedback.miloapis.com"
	SyncUser     = "system:feedback-sync@feedback.miloapis.com"
	UploadsUser  = "system:feedback-uploads@feedback.miloapis.com"
)

// Keys Milo sets in user extras.
const (
	ParentKindExtraKey    = "iam.miloapis.com/parent-type"
	ParentNameExtraKey    = "iam.miloapis.com/parent-name"
	EmailVerifiedExtraKey = "iam.miloapis.com/emailVerified"
)

// ParentKindUser is the parent kind of requests in a user's own context.
const ParentKindUser = "User"

// Caller is who a request comes from, as the feedback rules see it.
type Caller int

const (
	// Other is anyone else: staff, the website, users.
	Other Caller = iota
	Operator
	Sync
	Uploads
)

// Of returns the caller for u.
func Of(u authenticationv1.UserInfo) Caller {
	switch u.Username {
	case OperatorUser:
		return Operator
	case SyncUser:
		return Sync
	case UploadsUser:
		return Uploads
	}
	return Other
}

// User returns the user whose own context the request was made in, if it
// was made in one.
func User(u authenticationv1.UserInfo) (string, bool) {
	if !strings.EqualFold(extra(u, ParentKindExtraKey), ParentKindUser) {
		return "", false
	}
	name := extra(u, ParentNameExtraKey)
	return name, name != ""
}

// EmailUnverified reports whether the caller is a person whose email address
// isn't verified. Milo's identity provider sets the key on every human, "true"
// or "false"; machine identities don't carry it and pass, as in Milo's own
// admission plugin.
func EmailUnverified(u authenticationv1.UserInfo) bool {
	for k, vals := range u.Extra {
		if strings.EqualFold(k, EmailVerifiedExtraKey) {
			return !(len(vals) == 1 && vals[0] == "true")
		}
	}
	return false
}

// extra returns the single value of key, matched case-insensitively.
func extra(u authenticationv1.UserInfo, key string) string {
	for k, vals := range u.Extra {
		if strings.EqualFold(k, key) && len(vals) == 1 {
			return vals[0]
		}
	}
	return ""
}
