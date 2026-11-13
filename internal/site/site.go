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

// The OAuth paths.
const (
	DeviceCodePath = AccountHost + "/oauth2/device/code"
	TokenPath      = AccountHost + "/oauth2/token"
)

// The chat and model paths.
const (
	ModelsPath          = LLMHost + "/mavis/api/v1/models?region=en&buildEnv=prod"
	MessagesPath        = LLMHost + "/mavis/api/v1/llm/v1/messages"
	ChatCompletionsPath = LLMHost + "/mavis/api/v1/llm/v1/chat/completions"
	ResponsesPath       = LLMHost + "/mavis/api/v1/llm/v1/responses"
)

// The account API's paths.
const (
	IdentityPath   = AgentHost + "/v1/api/user/info"
	WorkspacePath  = AgentHost + "/matrix/api/v1/user/get_user_extra_info"
	MembershipPath = AgentHost + "/matrix/api/v1/commerce/get_membership_info"
	CreditPath     = AgentHost + "/minimax-cloud/api/v1/credit/details"
	SigninStatus   = AgentHost + "/minimax-cloud/api/v1/signin/status"
	SigninClaim    = AgentHost + "/minimax-cloud/api/v1/signin/claim"
)

// The plan console's paths.
const (
	PlanRemainsPath = PlatformHost + "/backend/account/token_plan/remains_percent"
	CodingPlanPath  = PlatformHost + "/v1/api/openplatform/coding_plan/remains"
	PlanTitlePath   = PlatformHost + "/v1/api/openplatform/charge/combo/cycle_audio_resource_package"
)
