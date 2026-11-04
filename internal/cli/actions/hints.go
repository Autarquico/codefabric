package actions

import "github.com/Autarquico/codefabric/internal/domain"

// hints say what a person can do about an error, by its code. The code stays the
// contract; the hint is only advice, and an unknown code has none.
var hints = map[domain.Code]string{
	domain.CodeControllerUnreachable: "Check that the Controller is running and reachable, then try again.",
	domain.CodeUnauthenticated:       "This machine is not enrolled or its credentials expired: run `cf connect` with a new invitation.",
	domain.CodeVersionIncompatible:   "Update cf or the Controller so their versions match.",
	domain.CodeNotFound:              "It no longer exists or the name is wrong: refresh the view and pick it again.",
	domain.CodeAmbiguousRef:          "More than one matches: use its ID or narrow it with a Project.",
	domain.CodeInvalidTransition:     "Its state changed in the meantime: refresh the view and check what it allows now.",
	domain.CodeStaleLease:            "The Run moved to another lease: refresh the view and try again.",
	domain.CodeInProgress:            "The same request is still being processed: wait a moment and look at the result.",
	domain.CodeIdempotencyMismatch:   "That request ID was used for something else: repeat the action with a new one.",
	domain.CodeNoEligibleWorker:      "No Worker can take it: bring one online or relax the requirements.",
	domain.CodeWorkerOffline:         "The Worker is offline: wait for it to return or choose another.",
	domain.CodeWorkerBusy:            "The Worker is busy: drain it or wait for its Runs to end.",
	domain.CodeWorkerCapacity:        "The Worker has no capacity left: choose another or free some.",
	domain.CodeWorkflowConflict:      "The Workflow changed under you: re-read it and apply again, rebasing if it asks.",
	domain.CodeRepoConfigInvalid:     "Fix .codefabric/project.yaml in the repository, or deny repo config for the Project.",
	domain.CodeSecretUnavailable:     "A secret it needs is not available on the Worker: check `cf project secrets`.",
	domain.CodePolicyDenied:          "This identity may not do that: ask an owner.",
	domain.CodeConfirmationRequired:  "Confirm the action to go on.",
	domain.CodeConfirmationStale:     "What you confirmed has changed: review it and confirm again.",
	domain.CodeCursorExpired:         "That page of history is gone: reload from the start.",
	domain.CodeNotImplemented:        "Not available in this version.",
	domain.CodeInvalidRequest:        "Check the values you gave and try again.",
	domain.CodeInternal:              "Not your doing: the Controller log has the details under the request ID.",
}

// attachHints refine CF_RUN_NOT_ATTACHABLE by the reason the Controller gave.
var attachHints = map[string]string{
	"headless":        "This Run is headless and has no session: follow its output in the Logs view.",
	"ssh_failed":      "ssh could not open the session: check you can ssh to the Worker, or set attach.ssh_targets in client.json.",
	"target_mismatch": "The Run is no longer where it was: refresh the view and try again.",
}

const attachHint = "The Run cannot be attached right now: it may have ended; refresh the view."

func hintFor(code domain.Code, details map[string]any) string {
	if code == domain.CodeRunNotAttachable {
		if reason, _ := details["reason"].(string); attachHints[reason] != "" {
			return attachHints[reason]
		}
		return attachHint
	}
	return hints[code]
}
