package cmd

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/autonomousops-co/accretion/internal/config"
)

// writeOmittedSecretsNotice reports the secrets GenerateDefault deliberately
// left out of the file and the variable that supplies each one (#129).
//
// Without this the omission is indistinguishable from an oversight: the
// operator gets a config that looks complete, and finds out which values are
// missing one failed startup at a time. Printing the list turns "this file is
// incomplete" into "these five values come from somewhere else, and here is
// where", which is the whole reason withholding them is affordable.
//
// It is a separate function taking an io.Writer so a test can assert every
// OmittedSecrets entry is named; see TestWriteOmittedSecretsNotice.
func writeOmittedSecretsNotice(w io.Writer) {
	fmt.Fprintf(w, "\nNo secret was written to it. Each of these comes from the environment instead —\n"+
		"the gitignored .env locally, a Kubernetes Secret via secretKeyRef in-cluster:\n\n")

	// tabwriter so the columns line up regardless of key length; a ragged list
	// of env vars is the kind of thing an operator skims past.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, secret := range config.OmittedSecrets {
		fmt.Fprintf(tw, "  %s\t%s\n", secret.Key, secret.EnvVar)
	}
	tw.Flush()

	fmt.Fprintf(w, "\nSet the ones your deployment uses. A missing session key or database password\n"+
		"is a startup failure that names the variable, not a silent fallback.\n")
}

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate configuration files",
	Long:  `Generate default configuration files for Accretion API`,
}

var generateConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Generate default configuration file",
	Long:  `Generate a default configuration file with all available options`,
	Run: func(cmd *cobra.Command, args []string) {
		outputFile, err := cmd.Flags().GetString("output")
		if err != nil {
			fmt.Printf("Error getting output flag: %v\n", err)
			os.Exit(1)
		}
		if outputFile == "" {
			outputFile = ".accretion-api.yaml"
		}

		if err := config.GenerateDefault(outputFile); err != nil {
			fmt.Printf("Error generating config: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Generated default configuration file: %s\n", outputFile)
		writeOmittedSecretsNotice(os.Stdout)
	},
}

func init() {
	generateConfigCmd.Flags().StringP("output", "o", "", "Output file path (default: .accretion-api.yaml)")
	generateCmd.AddCommand(generateConfigCmd)
	rootCmd.AddCommand(generateCmd)
}
