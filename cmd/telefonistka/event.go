package telefonistka

import (
	"os"

	"github.com/commercetools/telefonistka/internal/pkg/githubapi"
	"github.com/commercetools/telefonistka/internal/pkg/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/spf13/cobra"
)

// This is still(https://github.com/spf13/cobra/issues/1862) the documented way to use cobra
func init() { //nolint:gochecknoinits
	var eventType string
	var eventFilePath string
	var eventTimeout int
	eventCmd := &cobra.Command{
		Use:   "event",
		Short: "Handles a GitHub event based on event JSON file",
		Long:  "Handles a GitHub event based on event JSON file.\nThis operation mode was was built with GitHub Actions in mind",
		Args:  cobra.ExactArgs(0),
		Run: func(cmd *cobra.Command, args []string) {
			event(eventType, eventFilePath, eventTimeout)
		},
	}
	eventCmd.Flags().StringVarP(&eventType, "type", "t", getEnv("GITHUB_EVENT_NAME", ""), "Event type, defaults to GITHUB_EVENT_NAME env var")
	eventCmd.Flags().StringVarP(&eventFilePath, "file", "f", getEnv("GITHUB_EVENT_PATH", ""), "File path for event JSON, defaults to GITHUB_EVENT_PATH env var")
	eventCmd.Flags().IntVarP(&eventTimeout, "timeout", "e", utils.GetEnvInt("GITHUB_EVENT_TIMEOUT", 5), "Timeout in minutes for handling the event, defaults to 5 minutes")
	rootCmd.AddCommand(eventCmd)
}

func event(eventType string, eventFilePath string, eventTimeout int) {
	mainGhClientCache, _ := lru.New[string, githubapi.GhClientPair](128)
	prApproverGhClientCache, _ := lru.New[string, githubapi.GhClientPair](128)
	githubapi.ReciveEventFile(eventType, eventFilePath, mainGhClientCache, prApproverGhClientCache, eventTimeout)
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
