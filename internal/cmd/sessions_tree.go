package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsTreeCmd = &cobra.Command{
	Use:   "tree [<root-id>]",
	Short: "Show session hierarchy as an ASCII tree",
	Long: `Display sessions in a tree view showing parent-child relationships.

All sessions (including sub-agent children) are shown. Leaves show message
count and cost. Use --depth to limit recursion and --json for structured output.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Show all sessions as a tree
rush sessions tree

# Show tree for a specific root session
rush sessions tree my-session-id

# JSON output for scripting
rush sessions tree --json
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		depth, _ := cmd.Flags().GetInt("depth")
		asJSON, _ := cmd.Flags().GetBool("json")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		ctx := cmd.Context()

		sessions, err := a.Sessions.ListAll(ctx)
		if err != nil {
			return fmt.Errorf("failed to list sessions: %w", err)
		}

		// Build children map.
		children := make(map[string][]session.Session)
		var roots []session.Session
		for _, s := range sessions {
			if s.ParentSessionID == "" {
				roots = append(roots, s)
			} else {
				children[s.ParentSessionID] = append(children[s.ParentSessionID], s)
			}
		}

		// If root-id given, filter roots.
		if len(args) > 0 {
			sess, err := resolveSessionID(ctx, a.Sessions, args[0])
			if err != nil {
				return err
			}
			roots = []session.Session{sess}
		}

		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			for _, root := range roots {
				node := buildTreeNode(root, children, depth, 0)
				if err := enc.Encode(node); err != nil {
					return err
				}
			}
			return nil
		}

		for _, root := range roots {
			printTreeNode(os.Stdout, root, children, "", true, depth, 0)
		}
		return nil
	},
}

// treeJSON is the JSON output shape for sessions tree --json.
type treeJSON struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Msgs     int64      `json:"msgs"`
	Cost     float64    `json:"cost"`
	Children []treeJSON `json:"children,omitempty"`
}

func buildTreeNode(s session.Session, children map[string][]session.Session, maxDepth, currentDepth int) treeJSON {
	node := treeJSON{
		ID:    s.ID,
		Title: s.Title,
		Msgs:  s.MessageCount,
		// #1130: per-node OWN spend; the parent never carries the child's.
		Cost: s.OwnCost,
	}
	if maxDepth > 0 && currentDepth >= maxDepth {
		return node
	}
	ch := children[s.ID]
	node.Children = make([]treeJSON, len(ch))
	for i, c := range ch {
		node.Children[i] = buildTreeNode(c, children, maxDepth, currentDepth+1)
	}
	return node
}

func printTreeNode(w *os.File, s session.Session, children map[string][]session.Session, prefix string, isLast bool, maxDepth, currentDepth int) {
	connector := "├── "
	if isLast {
		connector = "└── "
	}
	if currentDepth == 0 {
		connector = ""
	}

	title := truncate(s.Title, 40)
	fmt.Fprintf(w, "%s%s%-40s (%d msgs, $%.2f)\n",
		prefix, connector, title, s.MessageCount, s.OwnCost)

	if maxDepth > 0 && currentDepth >= maxDepth {
		return
	}

	ch := children[s.ID]
	for i, c := range ch {
		last := i == len(ch)-1
		childPrefix := prefix
		if currentDepth > 0 {
			if isLast {
				childPrefix += "    "
			} else {
				childPrefix += "│   "
			}
		}
		printTreeNode(w, c, children, childPrefix, last, maxDepth, currentDepth+1)
	}
}

func init() {
	sessionsTreeCmd.Flags().Int("depth", 0, "Max tree depth (0 = unlimited)")
	sessionsTreeCmd.Flags().Bool("json", false, "Emit nested JSON instead of ASCII tree")
}
