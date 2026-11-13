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
	// UserAgent is what MiniMax Code calls itself.
	UserAgent = "MiniMaxAgent"
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
	IdentityPath   = "/v1/api/user/info"
	WorkspacePath  = "/matrix/api/v1/user/get_user_extra_info"
	MembershipPath = "/matrix/api/v1/commerce/get_membership_info"
	CreditPath     = "/minimax-cloud/api/v1/credit/details"
	SigninStatus   = "/minimax-cloud/api/v1/signin/status"
	SigninClaim    = "/minimax-cloud/api/v1/signin/claim"

	// The plan console's endpoints, on PlatformHost.
	PlanRemainsPath = "/backend/account/token_plan/remains_percent"
	CodingPlanPath  = "/v1/api/openplatform/coding_plan/remains"
	PlanTitlePath   = "/v1/api/openplatform/charge/combo/cycle_audio_resource_package"
)

// AccountURL gives an account endpoint's full URL.
func AccountURL(path string) string { return AccountHost + path }

// LLMURL gives a chat endpoint's full URL.
func LLMURL(path string) string { return LLMHost + path }

// PlatformURL gives a plan console endpoint's full URL.
func PlatformURL(path string) string { return PlatformHost + path }
