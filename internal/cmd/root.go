package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/autonomousops-co/accretion/internal/config"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "accretion-api",
	Short: "AI-powered Kubernetes log analysis service",
	Long: `Accretion API is a web service that consumes logs from pods in a Kubernetes cluster
and provides a UI for querying and displaying relevant insights using AI.`,
}

// Execute runs the root command. The version is passed in from main rather than
// read here, because -ldflags -X can only write to a variable in the package
// being linked as the binary's main.
func Execute(version string) {
	rootCmd.Version = version
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.accretion-api.yaml)")
}

// bootstrapLogLevel is the level for the temporary logger that exists only to
// report a config-load failure, before the configured level is known. Shared so
// the four subcommands that need it do not each repeat the literal.
const bootstrapLogLevel = "error"

func initConfig() {
	if cfgFile != "" {
		config.Debugf("DEBUG: Using config file from flag: %s\n", cfgFile)
		viper.SetConfigFile(cfgFile)
	} else {
		// Check for Kubernetes mounted config first
		config.Debugf("DEBUG: Checking for Kubernetes config at /etc/config/config.yaml\n")
		if _, err := os.Stat("/etc/config/config.yaml"); err == nil {
			config.Debugf("DEBUG: Found Kubernetes config, setting config file\n")
			viper.SetConfigFile("/etc/config/config.yaml")
		} else {
			config.Debugf("DEBUG: Kubernetes config not found (%v), using default paths\n", err)
			home, err := os.UserHomeDir()
			cobra.CheckErr(err)

			viper.AddConfigPath(home)
			viper.AddConfigPath(".")
			viper.SetConfigName(".accretion-api")
			viper.SetConfigType("yaml")
		}
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		config.Debugf("Using config file: %s\n", viper.ConfigFileUsed())
	} else {
		config.Debugf("DEBUG: Failed to read config file: %v\n", err)
	}
}
