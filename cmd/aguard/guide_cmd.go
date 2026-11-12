package main

import (
	"fmt"
	"os"

	"github.com/aiseeq/aguard/internal/guide"
)

func (a *app) guideCmd(args []string) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(a.out, guide.Render())
		return err
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "использование: aguard guide [вид|наблюдатель]")
		return exitCode(2)
	}
	letters, ok := guide.ByName(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "неизвестный вид %q; известные: %v\n", args[0], guide.Known())
		return exitCode(2)
	}
	_, err := fmt.Fprint(a.out, guide.RenderLetters(letters))
	return err
}
