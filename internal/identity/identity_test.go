package identity

import (
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestUser(t *testing.T) {
	for _, tc := range []struct {
		extra  map[string]authenticationv1.ExtraValue
		want   string
		wantOK bool
	}{
		{map[string]authenticationv1.ExtraValue{ParentKindExtraKey: {"User"}, ParentNameExtraKey: {"u1"}}, "u1", true},
		// Keys can arrive lower-cased.
		{map[string]authenticationv1.ExtraValue{"iam.miloapis.com/parent-type": {"user"}, "IAM.miloapis.com/Parent-Name": {"u1"}}, "u1", true},
		{map[string]authenticationv1.ExtraValue{ParentKindExtraKey: {"Organization"}, ParentNameExtraKey: {"o1"}}, "", false},
		{map[string]authenticationv1.ExtraValue{ParentKindExtraKey: {"User"}}, "", false},
		{nil, "", false},
	} {
		got, ok := User(authenticationv1.UserInfo{Extra: tc.extra})
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("User(%v) = %q, %v; want %q, %v", tc.extra, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestEmailUnverified(t *testing.T) {
	for _, tc := range []struct {
		vals []string
		set  bool
		want bool
	}{
		{[]string{"true"}, true, false},
		{[]string{"false"}, true, true},
		{[]string{""}, true, true},
		{nil, false, false}, // machines carry no verdict
	} {
		u := authenticationv1.UserInfo{}
		if tc.set {
			u.Extra = map[string]authenticationv1.ExtraValue{EmailVerifiedExtraKey: tc.vals}
		}
		if got := EmailUnverified(u); got != tc.want {
			t.Errorf("EmailUnverified(%v) = %v, want %v", tc.vals, got, tc.want)
		}
	}
}

func TestOf(t *testing.T) {
	if Of(authenticationv1.UserInfo{Username: OperatorUser}) != Operator || Of(authenticationv1.UserInfo{Username: "x"}) != Other {
		t.Error("Of misreads callers")
	}
}
