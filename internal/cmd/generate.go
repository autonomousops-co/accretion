package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/autonomousops-co/accretion/internal/config"
)

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
	},
}

func init() {
	generateConfigCmd.Flags().StringP("output", "o", "", "Output file path (default: .accretion-api.yaml)")
	generateCmd.AddCommand(generateConfigCmd)
	rootCmd.AddCommand(generateCmd)
}
