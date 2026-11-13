package site

import (
	"strings"
	"testing"
)

// TestPathsAreBare guards the convention: a path is a path and the host
// is a separate thing.
//
// A path that grew a host would break the gateway's ability to point
// the chats at another host, and would make every call site that
// composes host+path produce a doubled host.
func TestPathsAreBare(t *testing.T) {
	paths := []string{
		DeviceCodePath, TokenPath,
		ModelsPath, MessagesPath, ChatCompletionsPath, ResponsesPath,
		IdentityPath, WorkspacePath, MembershipPath, CreditPath,
		SigninStatus, SigninClaim,
		PlanRemainsPath, CodingPlanPath, PlanTitlePath,
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			t.Errorf("path %q does not start with a slash", p)
		}
		if strings.Contains(p, "://") {
			t.Errorf("path %q carries a host", p)
		}
	}
}

// TestURLHelpers checks that the helpers put the right host in front.
func TestURLHelpers(t *testing.T) {
	if got := AccountURL(DeviceCodePath); got != AccountHost+"/oauth2/device/code" {
		t.Errorf("AccountURL = %q", got)
	}
	if got := LLMURL(MessagesPath); got != LLMHost+"/mavis/api/v1/llm/v1/messages" {
		t.Errorf("LLMURL = %q", got)
	}
	if got := PlatformURL(PlanRemainsPath); got != PlatformHost+"/backend/account/token_plan/remains_percent" {
		t.Errorf("PlatformURL = %q", got)
	}
}

// TestHosts checks the hosts. Only the international site is here: the
// China site's hosts are a different set, and its tokens do not work on
// these.
func TestHosts(t *testing.T) {
	for name, h := range map[string]string{
		"AccountHost":  AccountHost,
		"LLMHost":      LLMHost,
		"AgentHost":    AgentHost,
		"PlatformHost": PlatformHost,
	} {
		if !strings.HasPrefix(h, "https://") {
			t.Errorf("%s = %q, want an https URL", name, h)
		}
		if strings.Contains(h, "minimax.cn") || strings.Contains(h, "minimaxi.com") {
			t.Errorf("%s = %q points at the China site", name, h)
		}
	}
}
