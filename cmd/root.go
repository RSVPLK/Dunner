package cmd

import (
	"fmt"
	"os"
	"github.com/spf13/cobra"
	"docker.io/go-docker"
	log "github.com/sirupsen/logrus"
)

var rootCmd = &cobra.Command{
	Use:   "dunner",
	Short: "Dunner is a Docker based task runner",
	Long:  `You can define a set of commands and on what Docker images these commands should run as steps. A task has many steps. Then you can run these tasks with 'dunner do nameoftask'`,
	Run: func(cmd *cobra.Command, args []string) {

		_, err := docker.NewEnvClient()
		if err != nil {
			log.Fatal(err)
		}

		fmt.Println("Dunner running!")
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}
