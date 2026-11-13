// Package site holds MiniMax Code's international site's hosts, paths
// and the constants its client sends.
//
// Only the international site is here. The China site (minimax.cn /
// minimaxi.com) is a different set of hosts whose tokens do not work on
// these, and it is out of scope.
package site

// The international site's hosts.
const (
	// AccountHost serves the OAuth device flow.
	AccountHost = "https://account.minimax.io"
	// LLMHost serves the chats and the model list.
	LLMHost = "https://agent.minimax.io"
	// AgentHost serves the account API, the check-in and the credits ledger.
	AgentHost = "https://agent.minimax.io"
	// PlatformHost serves the plan's windows.
	PlatformHost = "https://platform.minimax.io"
)

// The site's own region and language.
const (
	Region = "en"
	Lang   = "en"
)

// The OAuth client's parameters. These are MiniMax Code's own.
const (
	ClientID    = "mcode-public"
	Scope       = "agent.default"
	Audience    = "agent-backend"
	DeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
)

// The headers MiniMax Code's client sends with a chat.
const (
	// PlaceholderKey is the key MiniMax Code gives its SDK; the account
	// itself goes in Authorization.
	PlaceholderKey = "sk-xxx"
	// UserAgent is what MiniMax Code calls itself when it chats.
	UserAgent = "MiniMaxAgent"
	// AccountUserAgent is what MiniMax Code calls itself when it signs
	// an account API request.
	AccountUserAgent = "MiniMaxCode"
	// AgentID is MiniMax Code's agent id.
	AgentID = "main"
)

// The paths of the endpoints.
//
// They are paths, not URLs. The host is decided by whoever sends the
// request, so a test can point them at a stand-in server and a
// deployment can move a host without touching the paths.
//
// The full URL of an endpoint is its host followed by its path:
// site.AccountHost + site.DeviceCodePath.
const (
	// The OAuth endpoints, on AccountHost.
	DeviceCodePath = "/oauth2/device/code"
	TokenPath      = "/oauth2/token"

	// The chat and model endpoints, on LLMHost.
	ModelsPath          = "/mavis/api/v1/models"
	MessagesPath        = "/mavis/api/v1/llm/v1/messages"
	ChatCompletionsPath = "/mavis/api/v1/llm/v1/chat/completions"
	ResponsesPath       = "/mavis/api/v1/llm/v1/responses"

	// The account API's endpoints, on AgentHost.
	IdentityPath     = "/v1/api/user/info"
	WorkspacePath    = "/matrix/api/v1/user/get_user_extra_info"
	MembershipPath   = "/matrix/api/v1/commerce/get_membership_info"
	CreditPath       = "/minimax-cloud/api/v1/credit/details"
	SigninStatusPath = "/minimax-cloud/api/v1/signin/status"
	SigninClaimPath  = "/minimax-cloud/api/v1/signin/claim"

	// The plan console's endpoints, on PlatformHost.
	PlanRemainsPath = "/backend/account/token_plan/remains_percent"
	CodingPlanPath  = "/v1/api/openplatform/coding_plan/remains"
	PlanTitlePath   = "/v1/api/openplatform/charge/combo/cycle_audio_resource_package"
)

// ModelsQuery is the query the model list endpoint needs.
//
// It is the only endpoint with a query of its own, so it lives here
// rather than being spelled out at the call site.
const ModelsQuery = "region=" + Region + "&buildEnv=prod"

// KV is one query parameter.
//
// It is the site's own type, so that a package that builds a query does
// not have to know about the signing package to say what a parameter
// is. The signing package has one of its own, and the two are the same
// shape.
type KV struct {
	// K is the parameter's name.
	K string
	// V is the parameter's value.
	V string
}

// CheckinQuery is the query MiniMax Code's own check-in sends.
//
// The check-in comes from the desktop client, so it says so: the
// device_platform is "web", the desktop version is MiniMax Code's, and
// the account's own user id is in the query.
//
// It is kept here, once: the check-in and the credits ledger ask the
// same way, and the reference has one copy of it too.
var CheckinQuery = []KV{
	{K: "device_platform", V: "web"},
	{K: "is_desktop", V: "1"},
	{K: "desktop_version", V: "0.6.12"},
}

// AccountURL gives an account endpoint's full URL.
func AccountURL(path string) string { return AccountHost + path }

// AgentURL gives an account API endpoint's full URL.
//
// AgentHost and LLMHost are the same host today. That is exactly why
// this helper is worth having: a call site that picks the wrong one
// would not notice.
func AgentURL(path string) string { return AgentHost + path }

// LLMURL gives a chat endpoint's full URL.
func LLMURL(path string) string { return LLMHost + path }

// PlatformURL gives a plan console endpoint's full URL.
func PlatformURL(path string) string { return PlatformHost + path }
