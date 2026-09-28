// SPDX-License-Identifier: AGPL-3.0-or-later

package codexapp

const (
	methodInitialize                      = "initialize"
	methodInitialized                     = "initialized"
	methodAccountRead                     = "account/read"
	methodAccountLoginStart               = "account/login/start"
	methodAccountLoginCancel              = "account/login/cancel"
	methodAccountLoginCompleted           = "account/login/completed"
	methodAccountUpdated                  = "account/updated"
	methodModelList                       = "model/list"
	methodThreadStart                     = "thread/start"
	methodThreadResume                    = "thread/resume"
	methodThreadFork                      = "thread/fork"
	methodThreadRead                      = "thread/read"
	methodThreadStarted                   = "thread/started"
	methodThreadStatusChanged             = "thread/status/changed"
	methodThreadTokenUsageUpdated         = "thread/tokenUsage/updated"
	methodTurnStart                       = "turn/start"
	methodTurnSteer                       = "turn/steer"
	methodTurnInterrupt                   = "turn/interrupt"
	methodTurnStarted                     = "turn/started"
	methodTurnCompleted                   = "turn/completed"
	methodTurnDiffUpdated                 = "turn/diff/updated"
	methodTurnPlanUpdated                 = "turn/plan/updated"
	methodItemStarted                     = "item/started"
	methodItemCompleted                   = "item/completed"
	methodItemAgentMessageDelta           = "item/agentMessage/delta"
	methodItemCommandExecutionOutputDelta = "item/commandExecution/outputDelta"
	methodItemCommandExecutionApproval    = "item/commandExecution/requestApproval"
	methodItemFileChangeOutputDelta       = "item/fileChange/outputDelta"
	methodItemFileChangePatchUpdated      = "item/fileChange/patchUpdated"
	methodItemFileChangeApproval          = "item/fileChange/requestApproval"
	methodItemMCPToolCallProgress         = "item/mcpToolCall/progress"
	methodItemPermissionsApproval         = "item/permissions/requestApproval"
	methodItemPlanDelta                   = "item/plan/delta"
	methodItemReasoningSummaryPartAdded   = "item/reasoning/summaryPartAdded"
	methodItemReasoningSummaryTextDelta   = "item/reasoning/summaryTextDelta"
	methodItemReasoningTextDelta          = "item/reasoning/textDelta"
	methodItemToolCall                    = "item/tool/call"
	methodItemToolRequestUserInput        = "item/tool/requestUserInput"
	methodServerRequestResolved           = "serverRequest/resolved"
	methodError                           = "error"
)

var usedMethods = []string{
	methodAccountLoginCancel,
	methodAccountLoginCompleted,
	methodAccountLoginStart,
	methodAccountRead,
	methodAccountUpdated,
	methodError,
	methodInitialize,
	methodInitialized,
	methodItemAgentMessageDelta,
	methodItemCommandExecutionOutputDelta,
	methodItemCommandExecutionApproval,
	methodItemCompleted,
	methodItemFileChangeOutputDelta,
	methodItemFileChangePatchUpdated,
	methodItemFileChangeApproval,
	methodItemMCPToolCallProgress,
	methodItemPermissionsApproval,
	methodItemPlanDelta,
	methodItemReasoningSummaryPartAdded,
	methodItemReasoningSummaryTextDelta,
	methodItemReasoningTextDelta,
	methodItemStarted,
	methodItemToolCall,
	methodItemToolRequestUserInput,
	methodModelList,
	methodServerRequestResolved,
	methodThreadFork,
	methodThreadRead,
	methodThreadResume,
	methodThreadStart,
	methodThreadStarted,
	methodThreadStatusChanged,
	methodThreadTokenUsageUpdated,
	methodTurnCompleted,
	methodTurnDiffUpdated,
	methodTurnInterrupt,
	methodTurnPlanUpdated,
	methodTurnStart,
	methodTurnStarted,
	methodTurnSteer,
}
