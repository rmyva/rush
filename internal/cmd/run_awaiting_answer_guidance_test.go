package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestAwaitingAnswerResumeGuidancePassesRunRoleValidation(t *testing.T) {
	for _, role := range []string{"smart", "fast", "worker", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			guidance := agent.AwaitingAnswerGuidance("Which env should I deploy to?", nil, "sess-123", role)
			_, command, ok := strings.Cut(guidance, "rush run ")
			require.True(t, ok, "printed guidance must include a rush run command")
			command = strings.TrimSpace(strings.SplitN(command, "\n", 2)[0])
			args, err := parseResumeShellArgs("rush run " + command)
			require.NoError(t, err, "printed resume command must be valid shell arguments")
			require.GreaterOrEqual(t, len(args), 2)
			require.Equal(t, []string{"rush", "run"}, args[:2])

			flags := map[string]string{}
			prompt := make([]string, 0, len(args))
			for i := 2; i < len(args); i++ {
				arg := args[i]
				if !strings.HasPrefix(arg, "--") {
					prompt = append(prompt, arg)
					continue
				}
				i++
				require.Less(t, i, len(args), "flag %s must have a value", arg)
				flags[arg] = args[i]
			}
			require.Equal(t, "sess-123", flags["--session"])
			require.Equal(t, role, flags["--role"])
			_, err = validateRunRole(flags["--role"])
			require.NoError(t, err, "printed resume role must pass actual run validation")
			require.Equal(t, []string{"<your answer>"}, prompt)
		})
	}
}

func TestAwaitingAnswerGuidanceUnknownRoleOmitsResumeCommand(t *testing.T) {
	guidance := agent.AwaitingAnswerGuidance("Which env?", nil, "sess-123", "mystery")
	require.Contains(t, guidance, `role "mystery" is unknown`)
	require.NotContains(t, guidance, "rush run --role mystery")
	require.NotContains(t, guidance, "rush run ")
}

// parseResumeShellArgs decodes the simple POSIX shell quoting used by the
// guidance's resume command. It keeps quoted whitespace inside one argument.
func parseResumeShellArgs(command string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	started := false
	for _, r := range command {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote, started = r, true
		case ' ', '\t':
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		case '\\':
			return nil, fmt.Errorf("unsupported shell escape in %q", command)
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated shell quote in %q", command)
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}
