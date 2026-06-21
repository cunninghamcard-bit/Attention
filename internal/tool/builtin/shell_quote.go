package builtin

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

func shellJoin(args []string) (string, error) {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		item, err := shellQuote(arg)
		if err != nil {
			return "", err
		}
		quoted = append(quoted, item)
	}
	return strings.Join(quoted, " "), nil
}

func shellQuote(arg string) (string, error) {
	quoted, err := syntax.Quote(arg, syntax.LangPOSIX)
	if err != nil {
		return "", fmt.Errorf("quote shell argument %q: %w", arg, err)
	}
	return quoted, nil
}
