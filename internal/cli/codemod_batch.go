package cli

import (
	"fmt"
	"regexp"
	"strings"
)

// Rewriting drizzle's neon-http db.batch([...]) for postgres-js.
//
// neon-http's batch sends its statements as one non-interactive transaction
// and resolves to their results in order. postgres-js has no batch, but an
// interactive transaction running the same statements one after another has
// the same semantics - all or nothing, in order, results as a tuple:
//
//	const [users, posts] = await db.batch([db.select().from(u), db.insert(p).values(v)]);
//	const [users, posts] = await db.transaction(async (tx) => {
//	  const r0 = await tx.select().from(u);
//	  const r1 = await tx.insert(p).values(v);
//	  return [r0, r1] as [typeof r0, typeof r1];
//	});
//
// That rewrite is only mechanical when every statement is written inline as
// a query on the same client. Anything else - statements built elsewhere and
// passed in by variable, spreads, awaits inside the array, a receiver that is
// a property (this.db), code the scanner cannot read with certainty (regex
// literals) - is reported with the reason instead. Statements are rebound to
// the transaction client; running them on the outer client would put them
// outside the transaction.

// batchCallPattern finds `<identifier>.batch(`; the receiver must not itself
// be a property access (checked separately).
var batchCallPattern = regexp.MustCompile(`([A-Za-z_$][A-Za-z0-9_$]*)\s*\.\s*batch\s*\(`)

// drizzleStatementStart are the client methods that build a statement; an
// element starting with anything else is not known to be a drizzle query.
var drizzleStatementStart = regexp.MustCompile(`^(select|selectDistinct|selectDistinctOn|insert|update|delete|execute|query|with|\$with|\$count)\b`)

type batchRewriteNote struct {
	offset int
	reason string
}

// jsCodeMask marks which bytes of src are code (true) rather than inside a
// string, template text or comment. ok is false when the scanner met
// something it cannot classify with certainty (a '/' that could start a
// regex literal) or unbalanced quoting; callers must not rewrite then.
func jsCodeMask(src string) (mask []bool, ok bool) {
	mask = make([]bool, len(src))
	// Each entry is the brace depth at which an open ${...} substitution
	// closes; its closing brace resumes the template text.
	var substitutions []int
	braceDepth := 0
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			i += end
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += end + 4
			continue
		case c == '/':
			// Division or a regex literal: telling them apart needs a real
			// parser. Division is certain only directly after an identifier,
			// number or closing bracket.
			j := i - 1
			for j >= 0 && (src[j] == ' ' || src[j] == '\t') {
				j--
			}
			if j < 0 || (!isIdentByte(src[j]) && src[j] != ')' && src[j] != ']') {
				return nil, false
			}
		case c == '\'' || c == '"':
			end, closed := scanQuoted(src, i)
			if !closed {
				return nil, false
			}
			i = end
			continue
		case c == '`':
			next, entered, closed := scanTemplateText(src, i+1)
			if !closed {
				return nil, false
			}
			if entered {
				substitutions = append(substitutions, braceDepth)
			}
			i = next
			continue
		case c == '{':
			braceDepth++
		case c == '}':
			if len(substitutions) > 0 && braceDepth == substitutions[len(substitutions)-1] {
				substitutions = substitutions[:len(substitutions)-1]
				next, entered, closed := scanTemplateText(src, i+1)
				if !closed {
					return nil, false
				}
				if entered {
					substitutions = append(substitutions, braceDepth)
				}
				i = next
				continue
			}
			braceDepth--
		}
		mask[i] = true
		i++
	}
	if len(substitutions) > 0 {
		return nil, false
	}
	return mask, true
}

// scanTemplateText scans template-literal text from i up to the closing
// backtick (entered false) or the next ${ (entered true), returning the index
// just past it.
func scanTemplateText(src string, i int) (next int, entered, ok bool) {
	for i < len(src) {
		switch {
		case src[i] == '\\':
			i += 2
		case src[i] == '`':
			return i + 1, false, true
		case src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			return i + 2, true, true
		default:
			i++
		}
	}
	return i, false, false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// scanQuoted returns the index just past the string literal starting at i.
func scanQuoted(src string, i int) (int, bool) {
	quote := src[i]
	i++
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case '\n':
			return i, false
		case quote:
			return i + 1, true
		}
		i++
	}
	return i, false
}

// matchBracket returns the index of the bracket closing the one at open,
// counting only code bytes.
func matchBracket(src string, mask []bool, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		if !mask[i] {
			continue
		}
		switch src[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitTopLevel splits src[start:end] at commas outside any bracket.
func splitTopLevel(src string, mask []bool, start, end int) []string {
	var parts []string
	depth := 0
	last := start
	for i := start; i < end; i++ {
		if !mask[i] {
			continue
		}
		switch src[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, src[last:i])
				last = i + 1
			}
		}
	}
	parts = append(parts, src[last:end])
	return parts
}

// rebindReceiver replaces code occurrences of `receiver.` (not preceded by a
// property access or identifier character) with `tx.`.
func rebindReceiver(element string, receiver, tx string) (string, bool) {
	mask, ok := jsCodeMask(element)
	if !ok {
		return "", false
	}
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(receiver) + `\s*\.`)
	var b strings.Builder
	last := 0
	for _, match := range pattern.FindAllStringIndex(element, -1) {
		start := match[0]
		if !mask[start] {
			continue
		}
		if start > 0 && (element[start-1] == '.' || isIdentByte(element[start-1])) {
			continue
		}
		b.WriteString(element[last:start])
		b.WriteString(tx + ".")
		last = match[1]
	}
	b.WriteString(element[last:])
	return b.String(), true
}

// freshName returns base, then base1, base2..., whichever first does not
// appear as a word in any of texts.
func freshName(texts []string, base string) string {
	joined := strings.Join(texts, "\n")
	for n := 0; ; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s%d", base, n)
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(joined) {
			return name
		}
	}
}

// freshResultBase returns a prefix p such that no word p<digits> appears in
// texts, so the numbered result constants cannot shadow anything the
// statements reference.
func freshResultBase(texts []string) string {
	joined := strings.Join(texts, "\n")
	for n := 0; ; n++ {
		base := "r"
		if n > 0 {
			base = fmt.Sprintf("r%d_", n)
		}
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(base) + `[0-9]+\b`).MatchString(joined) {
			return base
		}
	}
}

// rewriteDrizzleBatches rewrites every mechanically safe db.batch([...]) in
// src and returns the new source, the number of rewrites, and a note for
// every batch call it left alone.
func rewriteDrizzleBatches(src string, typescript bool) (string, int, []batchRewriteNote) {
	mask, ok := jsCodeMask(src)
	if !ok {
		var notes []batchRewriteNote
		for _, match := range batchCallPattern.FindAllStringIndex(src, -1) {
			notes = append(notes, batchRewriteNote{offset: match[0], reason: "db.batch() not rewritten: the file has syntax the codemod cannot read with certainty (e.g. a regex literal); rewrite to db.transaction(async (tx) => { ... }) by hand and rebind each statement to tx"})
		}
		return src, 0, notes
	}

	var out strings.Builder
	var notes []batchRewriteNote
	rewrites := 0
	last := 0
	for _, match := range batchCallPattern.FindAllStringSubmatchIndex(src, -1) {
		start, openParen := match[0], match[1]-1
		if start < last || !mask[start] {
			continue
		}
		receiver := src[match[2]:match[3]]
		note := func(reason string) {
			notes = append(notes, batchRewriteNote{offset: start, reason: "db.batch() not rewritten: " + reason + "; rewrite to " + receiver + ".transaction(async (tx) => { ... }) by hand and rebind each statement to tx"})
		}
		if start > 0 && src[start-1] == '.' {
			note("the client is a property (" + receiver + " is reached through another object)")
			continue
		}
		closeParen := matchBracket(src, mask, openParen)
		if closeParen < 0 {
			note("unbalanced brackets")
			continue
		}
		args := strings.TrimSpace(src[openParen+1 : closeParen])
		if !strings.HasPrefix(args, "[") {
			note("the statements are not an inline array")
			continue
		}
		openBracket := openParen + 1 + strings.Index(src[openParen+1:closeParen], "[")
		closeBracket := matchBracket(src, mask, openBracket)
		if closeBracket < 0 || strings.Trim(src[closeBracket+1:closeParen], " \t\r\n,") != "" {
			note("the call has more than one argument")
			continue
		}

		var elements []string
		for _, part := range splitTopLevel(src, mask, openBracket+1, closeBracket) {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				elements = append(elements, trimmed)
			}
		}
		if len(elements) == 0 {
			note("the batch is empty")
			continue
		}
		prefix := regexp.MustCompile(`^` + regexp.QuoteMeta(receiver) + `\s*\.\s*`)
		reason := ""
		for _, element := range elements {
			switch {
			case strings.HasPrefix(element, "..."):
				reason = "a statement is spread into the array"
			case regexp.MustCompile(`\bawait\b`).MatchString(element):
				reason = "a statement contains await"
			case !prefix.MatchString(element):
				reason = "a statement is not written inline on " + receiver + " (built elsewhere and passed in)"
			case !drizzleStatementStart.MatchString(prefix.ReplaceAllString(element, "")):
				reason = "a statement does not start with a drizzle query builder method"
			}
			if reason != "" {
				break
			}
		}
		if reason != "" {
			note(reason)
			continue
		}

		tx := freshName(elements, "tx")
		rebound := make([]string, len(elements))
		for i, element := range elements {
			value, ok := rebindReceiver(element, receiver, tx)
			if !ok {
				reason = "a statement has syntax the codemod cannot read with certainty"
				break
			}
			rebound[i] = value
		}
		if reason != "" {
			note(reason)
			continue
		}

		indent := lineIndent(src, start)
		names := make([]string, len(elements))
		resultBase := freshResultBase(elements)
		var body strings.Builder
		fmt.Fprintf(&body, "%s.transaction(async (%s) => {\n", receiver, tx)
		for i, element := range rebound {
			names[i] = fmt.Sprintf("%s%d", resultBase, i)
			fmt.Fprintf(&body, "%s  const %s = await %s;\n", indent, names[i], element)
		}
		if typescript {
			types := make([]string, len(names))
			for i, name := range names {
				types[i] = "typeof " + name
			}
			fmt.Fprintf(&body, "%s  return [%s] as [%s];\n", indent, strings.Join(names, ", "), strings.Join(types, ", "))
		} else {
			fmt.Fprintf(&body, "%s  return [%s];\n", indent, strings.Join(names, ", "))
		}
		fmt.Fprintf(&body, "%s})", indent)

		out.WriteString(src[last:start])
		out.WriteString(body.String())
		last = closeParen + 1
		rewrites++
	}
	out.WriteString(src[last:])
	return out.String(), rewrites, notes
}

// lineIndent returns the leading whitespace of the line containing offset.
func lineIndent(src string, offset int) string {
	lineStart := strings.LastIndexByte(src[:offset], '\n') + 1
	end := lineStart
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return src[lineStart:end]
}
