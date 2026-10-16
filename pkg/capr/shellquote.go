package capr

import (
	"regexp"
	"strings"
	"unicode"
)

// This file holds the helpers that put user supplied values into text that a shell or PowerShell parses.
// Use them for every such value. Do not build quotes with fmt.Sprintf("%q") or "\"%s\"".
//
//   - Paths in "sh -c" strings of a plan: ShellQuote. A normal path stays unchanged, so plans keep the same bytes.
//   - Values in a script or a command that the user copies: ShellQuoteValue or PowerShellQuoteValue.
//     They keep the double quoted form that existing output uses when the value is plain.
//   - Always single quoted output: ShellQuoteAlways or PowerShellQuote.
//   - Names of variables: check IsValidEnvVarName before the name goes into a statement.

var validEnvVarName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// IsValidEnvVarName returns true if name is a valid POSIX shell variable name.
// Callers must check it before they write a user supplied name into a shell or PowerShell statement.
func IsValidEnvVarName(name string) bool {
	return validEnvVarName.MatchString(name)
}

// ShellQuote returns s as one literal word for a POSIX shell. It returns s unchanged when every
// character is a letter, a digit, or one of "_@+:,./-", so existing plans for normal paths keep
// the same bytes. Otherwise, it wraps s in single quotes and escapes each single quote.
func ShellQuote(s string) string {
	safe := s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !strings.ContainsRune("_@+:,./-", r)
	})
	if safe {
		return s
	}
	return ShellQuoteAlways(s)
}

// ShellQuoteAlways returns s as one literal word for a POSIX shell. It always wraps s in single quotes
// and escapes each single quote.
func ShellQuoteAlways(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuoteValue returns v as one literal word for a POSIX shell. It keeps the double quoted form
// for a value that the shell cannot expand. Inside double quotes a shell expands "$", "`", "\" and "!",
// and a double quote ends the string. Any other value goes into single quotes.
func ShellQuoteValue(v string) string {
	if strings.ContainsAny(v, "\"$`\\!\r\n") {
		return ShellQuoteAlways(v)
	}
	return `"` + v + `"`
}

// PowerShellQuote returns s as one literal string for PowerShell. It uses single quotes and doubles
// every character that PowerShell treats as a single quote, including the Unicode curly quotes.
func PowerShellQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		b.WriteRune(r)
		switch r {
		case '\'', '\u2018', '\u2019', '\u201A', '\u201B':
			// PowerShell escapes a quote inside a single-quoted string by doubling it.
			// For example, it's becomes it''s.
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// PowerShellQuoteValue returns v as one literal string for PowerShell. It keeps the double quoted form
// for a value that PowerShell cannot expand. Inside double quotes PowerShell expands "$" and the backtick,
// and a double quote ends the string. PowerShell also reads the curly double quotes as double quotes.
// Any other value goes into single quotes.
func PowerShellQuoteValue(v string) string {
	if strings.ContainsAny(v, "\"$`\u201C\u201D\u201E\r\n") {
		return PowerShellQuote(v)
	}
	return `"` + v + `"`
}
