package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lingoo12138/mmxcode/internal/site"
)

func newModelsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "models",
		Short: "列出可用模型",
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, m := range site.FallbackModels() {
				fmt.Printf("%-28s %8d %8d  %s\n", m.ID, m.Context, m.Output, m.Thinking)
			}
			return nil
		},
	}
}
