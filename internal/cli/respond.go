package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// newTaskRespondCmd is `cf task respond`: it answers what a Task's Run waits on.
func newTaskRespondCmd() *cobra.Command {
	var (
		req         v1.RespondRequest
		allow, deny bool
	)
	cmd := &cobra.Command{
		Use:   "respond <task>",
		Short: "Answer the permission or question a Task's Run waits on",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			answer, err := answerOf(allow, deny, req.Answer.Text)
			if err != nil {
				return err
			}
			req.Answer = answer
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.RespondTask(cmd.Context(), args[0], refScope(cmd, c), req)
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Answered %s of %s; it goes back to running once the Worker delivers it.\n", out.PromptID, out.RunShort)
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.PromptID, "prompt", "", "prompt to answer (default: the only one waiting)")
	f.StringVar(&req.Run, "run", "", "Run whose prompt to answer, when more than one Run of the Task waits (default: the only one)")
	f.BoolVar(&allow, "allow", false, "allow what a permission prompt asks")
	f.BoolVar(&deny, "deny", false, "deny what a permission prompt asks")
	f.StringVar(&req.Answer.Text, "text", "", "answer to a question or free-text prompt")
	f.StringVar(&req.RequestID, flagRequestID, "", "idempotency key: repeating the command with it delivers the answer once")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

// answerOf builds the answer from the flags, exactly one of which is given.
func answerOf(allow, deny bool, text string) (v1.InputAnswer, error) {
	given := 0
	for _, g := range []bool{allow, deny, text != ""} {
		if g {
			given++
		}
	}
	if given != 1 {
		return v1.InputAnswer{}, domain.NewError(domain.CodeInvalidRequest, "give exactly one of --allow, --deny or --text", nil)
	}
	switch {
	case allow:
		return v1.InputAnswer{Decision: domain.AnswerAllow}, nil
	case deny:
		return v1.InputAnswer{Decision: domain.AnswerDeny}, nil
	}
	return v1.InputAnswer{Text: text}, nil
}

// newInboxCmd is `cf inbox`: what needs a person.
func newInboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inbox",
		Short: "List what needs you: Tasks waiting for input and failures to decide on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.Inbox(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error { return printInbox(w, out.Items) })
		},
	}
}

func printInbox(out io.Writer, items []v1.InboxItem) error {
	w := newTable(out, "KIND\tTASK\tRUN\tPROMPT\tDETAIL\tSINCE")
	for _, it := range items {
		prompt, detail := "-", "-"
		switch {
		case it.Prompt != nil:
			prompt, detail = it.Prompt.ID+" ("+string(it.Prompt.Kind)+")", it.Prompt.Summary
			if !it.Run.Respondable {
				detail += " [attach: cf attach " + it.Run.Short + "]"
			}
		case it.EndReason != "":
			detail = string(it.EndReason)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Kind, printable(it.Task.Name), it.Run.Short, printable(prompt), printable(detail), local(it.Since))
	}
	return w.Flush()
}
