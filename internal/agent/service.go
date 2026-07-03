package agent

import (
	"context"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// AdapterService is the owner surface used by user-facing adapters.
type AdapterService interface {
	SetEventHandler(func(Event))
	Init(context.Context)

	CurrentWarnings() []PromptWarning
	SubmitToSession(context.Context, string, string) (SubmitResult, error)
	QueueSnapshotForSession(string) (QueueState, error)
	CancelSession(string) error
	CompactNowForSession(context.Context, string) error

	RespondPermissionActionForSession(string, string, string) error
	PermissionSuggestForSession(string, string, string) ([]PermissionSuggestion, error)
	PermissionSuggestForProject(string, string, string) ([]PermissionSuggestion, error)
	SaveProjectPermissionForSession(string, string, []string) error

	SwitchModelForSession(string, string) error
	CurrentModelForSession(string) (ModelInfo, error)
	ModelList() []ModelListEntry
	AllModelList() []ModelListEntry
	SetDefaultModel(string) error
	SetModelHidden(string, bool) error
	SetProviderHidden(string, bool) error
	CompleteModelEntry(string, ModelCompletion) error

	ProviderList() []ProviderStatus
	ConnectProvider(string, string) error
	DiscoverCustomProvider(CustomProviderRequest) ([]DiscoveryModelCandidate, error)
	AddCustomProvider(CustomProviderRequest) error
	DisconnectProvider(string) error
	RemoveProvider(string) error
	GenerateAPIKeyEnvName(string) string
	GetProviderConfig(string) (ProviderConfigView, error)
	DiscoverableModels(string) ([]DiscoveryModelCandidate, error)
	SetProviderConfig(string, ProviderConfigInput) error
	ResetProviderField(string, string) error
	SaveModel(string, string, ModelConfigInput) error
	DeleteModel(string, string) error
	ResetModelField(string, string, string) error

	Reload() error
	GetRuntimeConfig() RuntimeConfigSettings
	SetRuntimeConfig(RuntimeConfigSettings) error

	TokenUsageForSession(string) (TokenReport, error)
	SessionSummaryForSession(string) (SessionSummary, error)
	SessionPayloadForSession(string) (SessionPayload, error)
	SessionList(string) ([]SessionSummary, error)
	OpenSession(string) (SessionSummary, error)
	NewSession(string, string) (string, error)
	NewSessionForProjectPath(string, string) (string, error)
	SessionArchive(string) error
	SessionDelete(string) error
	SessionMessagesFor(string) ([]DisplayMessage, error)
	ApplyTurnActionForSession(string, int, string, bool) (TurnActionResult, error)
	RevertCodeForSession(string, int) (snapshot.RevertResult, error)
	RevertHistoryForSession(string, int) error
	SnapshotListForSession(string) ([]Snapshot, error)

	ReadFileContent(string) (string, error)
	ReadFileContentForProjectPath(string, string) (string, error)
	ProjectName() string
	ProjectRoot() string
	ProjectCurrent() ProjectSummary
	ProjectCurrentForPath(string) (ProjectSummary, error)
	ProjectList() ([]ProjectSummary, error)
	SessionListForProjectPath(string, string) ([]SessionSummary, error)
}

var _ AdapterService = (*Agent)(nil)
