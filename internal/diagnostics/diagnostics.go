package diagnostics

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/mitchellh/colorstring"
	"github.com/mitchellh/go-wordwrap"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcled"
	"github.com/hashicorp/hcl/v2/hclparse"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/hashicorp/enos/proto/hashicorp/enos/v1/pb"
)

// HasErrors returns true if any diagnostic has an error severity.
func HasErrors(diags []*pb.Diagnostic) bool {
	if len(diags) < 1 {
		return false
	}

	for _, diag := range diags {
		if diag.Severity == pb.Diagnostic_SEVERITY_ERROR {
			return true
		}
	}

	return false
}

// HasWarnings returns true if any diagnostic has a warning severity.
func HasWarnings(diags []*pb.Diagnostic) bool {
	if len(diags) < 1 {
		return false
	}

	for _, diag := range diags {
		if diag.Severity == pb.Diagnostic_SEVERITY_WARNING {
			return true
		}
	}

	return false
}

// Concat takes one-or-more sets of daignostics and returns a combined set
func Concat(diags ...[]*pb.Diagnostic) []*pb.Diagnostic {
	combined := []*pb.Diagnostic{}
	for _, diag := range diags {
		combined = append(combined, diag...)
	}

	return combined
}

// FromErr takes a standard go error and returns proto diagnostics
func FromErr(err error) []*pb.Diagnostic {
	if err == nil {
		return nil
	}

	return []*pb.Diagnostic{{
		Severity: pb.Diagnostic_SEVERITY_ERROR,
		Summary:  err.Error(),
	}}
}

// FromTFJSON takes terraform-json Diagnostics and returns them as proto diagnostics
func FromTFJSON(in []tfjson.Diagnostic) []*pb.Diagnostic {
	if len(in) < 1 {
		return nil
	}

	out := []*pb.Diagnostic{}
	for _, din := range in {
		d := &pb.Diagnostic{
			Summary: din.Summary,
			Detail:  din.Detail,
			Range: &pb.Range{
				Filename: din.Range.Filename,
				Start: &pb.Range_Pos{
					Line:   int64(din.Range.Start.Line),
					Column: int64(din.Range.Start.Column),
					Byte:   int64(din.Range.Start.Byte),
				},
				End: &pb.Range_Pos{
					Line:   int64(din.Range.End.Line),
					Column: int64(din.Range.End.Column),
					Byte:   int64(din.Range.End.Byte),
				},
			},
			Snippet: &pb.Diagnostic_Snippet{
				Context:              *din.Snippet.Context,
				Code:                 din.Snippet.Code,
				StartLine:            int64(din.Snippet.StartLine),
				HighlightStartOffset: int64(din.Snippet.HighlightStartOffset),
				HighlightEndOffset:   int64(din.Snippet.HighlightEndOffset),
			},
		}

		switch din.Severity {
		case tfjson.DiagnosticSeverityError:
			d.Severity = pb.Diagnostic_SEVERITY_ERROR
		case tfjson.DiagnosticSeverityWarning:
			d.Severity = pb.Diagnostic_SEVERITY_WARNING
		default:
			d.Severity = pb.Diagnostic_SEVERITY_UNKNOWN
		}

		snippet := &pb.Diagnostic_Snippet{
			Context:              *din.Snippet.Context,
			Code:                 din.Snippet.Code,
			StartLine:            int64(din.Snippet.StartLine),
			HighlightStartOffset: int64(din.Snippet.HighlightStartOffset),
			HighlightEndOffset:   int64(din.Snippet.HighlightEndOffset),
			Values:               []*pb.Diagnostic_ExpressionValue{},
		}
		for i, expr := range din.Snippet.Values {
			if i == 0 {
				snippet.Values = []*pb.Diagnostic_ExpressionValue{}
			}
			snippet.Values = append(snippet.Values, &pb.Diagnostic_ExpressionValue{
				Traversal: expr.Traversal,
				Statement: expr.Statement,
			})
		}
		d.Snippet = snippet

		out = append(out, d)
	}

	return out
}

// FromHCL takes a map of hcl.Files and hcl.Diagnostics and returns pb diagnostics.
// When possible it will attempt to create a valid snippet.
func FromHCL(files map[string]*hcl.File, diags hcl.Diagnostics) []*pb.Diagnostic {
	if len(diags) < 1 {
		return nil
	}

	res := []*pb.Diagnostic{}
	for _, diag := range diags {
		pbDiag := &pb.Diagnostic{
			Summary: diag.Summary,
			Detail:  diag.Detail,
		}

		switch diag.Severity {
		case hcl.DiagError:
			pbDiag.Severity = pb.Diagnostic_SEVERITY_ERROR
		case hcl.DiagWarning:
			pbDiag.Severity = pb.Diagnostic_SEVERITY_WARNING
		default:
			pbDiag.Severity = pb.Diagnostic_SEVERITY_UNKNOWN
		}

		// If we actually have a file that matches our diag subjec5t
		if diag.Subject != nil {
			highlightRange := *diag.Subject

			// Some diagnostic sources fail to set the end of the subject range.
			if highlightRange.End == (hcl.Pos{}) {
				highlightRange.End = highlightRange.Start
			}

			snippetRange := highlightRange
			if diag.Context != nil {
				snippetRange = *diag.Context
			}

			// Make sure the snippet includes the highlight. This should be true
			// for any reasonable diagnostic, but we'll make sure.
			snippetRange = hcl.RangeOver(snippetRange, highlightRange)
			if snippetRange.Empty() {
				snippetRange.End.Byte++
				snippetRange.End.Column++
			}
			if highlightRange.Empty() {
				highlightRange.End.Byte++
				highlightRange.End.Column++
			}

			pbDiag.Range = hclRangeToProtoRange(highlightRange)

			file := files[diag.Subject.Filename]
			if file != nil && file.Bytes != nil {
				pbDiag.Snippet = &pb.Diagnostic_Snippet{
					StartLine: int64(snippetRange.Start.Line),
				}

				file, offset := parseRange(file.Bytes, highlightRange)

				// Some diagnostics may have a useful top-level context to add to
				// the code snippet output.
				contextStr := hcled.ContextString(file, offset-1)
				if contextStr != "" {
					pbDiag.Snippet.Context = contextStr
				}

				// Build the string of the code snippet, tracking at which byte of
				// the file the snippet starts.
				var codeStartByte int
				sc := hcl.NewRangeScanner(file.Bytes, highlightRange.Filename, bufio.ScanLines)
				var code strings.Builder
				for sc.Scan() {
					lineRange := sc.Range()
					if lineRange.Overlaps(snippetRange) {
						if codeStartByte == 0 && code.Len() == 0 {
							codeStartByte = lineRange.Start.Byte
						}
						code.Write(lineRange.SliceBytes(file.Bytes))
						code.WriteRune('\n')
					}
				}
				codeStr := strings.TrimSuffix(code.String(), "\n")
				pbDiag.Snippet.Code = codeStr

				// Calculate the start and end byte of the highlight range relative
				// to the code snippet string.
				start := highlightRange.Start.Byte - codeStartByte
				end := start + (highlightRange.End.Byte - highlightRange.Start.Byte)

				// We can end up with some quirky results here in edge cases like
				// when a source range starts or ends at a newline character,
				// so we'll cap the results at the bounds of the highlight range
				// so that consumers of this data don't need to contend with
				// out-of-bounds errors themselves.
				if start < 0 {
					start = 0
				} else if start > len(codeStr) {
					start = len(codeStr)
				}
				if end < 0 {
					end = 0
				} else if end > len(codeStr) {
					end = len(codeStr)
				}

				pbDiag.Snippet.HighlightStartOffset = int64(start)
				pbDiag.Snippet.HighlightEndOffset = int64(end)

				if diag.Expression != nil {
					// We may also be able to generate information about the dynamic
					// values of relevant variables at the point of evaluation, then.
					// This is particularly useful for expressions that get evaluated
					// multiple times with different values, such as blocks using
					// "count" and "for_each", or within "for" expressions.
					expr := diag.Expression
					ctx := diag.EvalContext
					vars := expr.Variables()
					values := make([]*pb.Diagnostic_ExpressionValue, 0, len(vars))
					seen := make(map[string]struct{}, len(vars))
				Traversals:
					for _, traversal := range vars {
						for len(traversal) > 1 {
							val, diags := traversal.TraverseAbs(ctx)
							if diags.HasErrors() {
								// Skip anything that generates errors, since we probably
								// already have the same error in our diagnostics set
								// already.
								traversal = traversal[:len(traversal)-1]
								continue
							}

							traversalStr := traversalStr(traversal)
							if _, exists := seen[traversalStr]; exists {
								continue Traversals // don't show duplicates when the same variable is referenced multiple times
							}
							value := &pb.Diagnostic_ExpressionValue{
								Traversal: traversalStr,
							}
							switch {
							case !val.IsKnown():
								if ty := val.Type(); ty != cty.DynamicPseudoType {
									value.Statement = fmt.Sprintf("is a %s, known only after apply", ty.FriendlyName())
								} else {
									value.Statement = "will be known only after apply"
								}
							default:
								value.Statement = fmt.Sprintf("is %s", compactValueStr(val))
							}
							values = append(values, value)
							seen[traversalStr] = struct{}{}
						}
					}
					sort.Slice(values, func(i, j int) bool {
						return values[i].Traversal < values[j].Traversal
					})
					pbDiag.Snippet.Values = values
				}
			}
		}

		res = append(res, pbDiag)
	}

	return res
}

type stringOptConfig struct {
	showSnippet bool
	color       *colorstring.Colorize
	uiSettings  *pb.UI_Settings
}

// StringOpt is an option to the string formatter
type StringOpt func(*stringOptConfig)

// WithStringSnippetEnabled enables or diables the snippet in the formatting
func WithStringSnippetEnabled(enabled bool) StringOpt {
	return func(cfg *stringOptConfig) {
		cfg.showSnippet = enabled
	}
}

// WithStringUISettings passes UI settings to the string formatter
func WithStringUISettings(settings *pb.UI_Settings) StringOpt {
	return func(cfg *stringOptConfig) {
		cfg.uiSettings = settings
	}
}

// WithStringColor passes color settings to the formatter
func WithStringColor(color *colorstring.Colorize) StringOpt {
	return func(cfg *stringOptConfig) {
		cfg.color = color
	}
}

// String writes the diagnostic as a string. It takes optional configuration
// settings to modify the format.
func String(diag *pb.Diagnostic, opts ...StringOpt) string {
	cfg := &stringOptConfig{
		showSnippet: true,
		color: &colorstring.Colorize{
			Colors: colorstring.DefaultColors,
		},
	}
	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.uiSettings != nil {
		cfg.color.Disable = cfg.uiSettings.GetUseColor()
	}

	if diag == nil {
		return ""
	}

	var buf bytes.Buffer
	var leftRuleLine, leftRuleStart, leftRuleEnd string
	var leftRuleWidth int // in visual character cells
	var width int
	if cfg.uiSettings != nil {
		width = int(cfg.uiSettings.GetWidth())
	}

	switch diag.Severity {
	case pb.Diagnostic_SEVERITY_ERROR:
		buf.WriteString(cfg.color.Color("[bold][red]Error: [reset]"))
		leftRuleLine = cfg.color.Color("[red]│[reset] ")
		leftRuleStart = cfg.color.Color("[red]╷[reset]")
		leftRuleEnd = cfg.color.Color("[red]╵[reset]")
		leftRuleWidth = 2
	case pb.Diagnostic_SEVERITY_WARNING:
		buf.WriteString(cfg.color.Color("[bold][yellow]Warning: [reset]"))
		leftRuleLine = cfg.color.Color("[yellow]│[reset] ")
		leftRuleStart = cfg.color.Color("[yellow]╷[reset]")
		leftRuleEnd = cfg.color.Color("[yellow]╵[reset]")
		leftRuleWidth = 2
	default:
		buf.WriteString(cfg.color.Color("\n[reset]"))
	}

	// We don't wrap the summary, since we expect it to be terse, and since
	// this is where we put the text of a native Go error it may not always
	// be pure text that lends itself well to word-wrapping.
	fmt.Fprintf(&buf, cfg.color.Color("[bold]%s[reset]\n\n"), diag.Summary)

	appendSourceSnippets(&buf, diag, cfg.color)

	if diag.Detail != "" {
		paraWidth := width - leftRuleWidth - 1 // leave room for the left rule
		if paraWidth > 0 {
			lines := strings.Split(diag.Detail, "\n")
			for _, line := range lines {
				if !strings.HasPrefix(line, " ") {
					line = wordwrap.WrapString(line, uint(paraWidth))
				}
				fmt.Fprintf(&buf, "%s\n", line)
			}
		} else {
			fmt.Fprintf(&buf, "%s\n", diag.Detail)
		}
	}

	// Before we return, we'll finally add the left rule prefixes to each
	// line so that the overall message is visually delimited from what's
	// around it. We'll do that by scanning over what we already generated
	// and adding the prefix for each line.
	var ruleBuf strings.Builder
	sc := bufio.NewScanner(&buf)
	ruleBuf.WriteString(leftRuleStart)
	ruleBuf.WriteByte('\n')
	for sc.Scan() {
		line := sc.Text()
		prefix := leftRuleLine
		if line == "" {
			// Don't print the space after the line if there would be nothing
			// after it anyway.
			prefix = strings.TrimSpace(prefix)
		}
		ruleBuf.WriteString(prefix)
		ruleBuf.WriteString(line)
		ruleBuf.WriteByte('\n')
	}
	ruleBuf.WriteString(leftRuleEnd)
	ruleBuf.WriteByte('\n')

	return ruleBuf.String()
}

func appendSourceSnippets(buf *bytes.Buffer, diag *pb.Diagnostic, color *colorstring.Colorize) {
	if diag.Range == nil {
		return
	}

	if diag.Snippet == nil {
		// This should generally not happen, as long as sources are always
		// loaded through the main loader. We may load things in other
		// ways in weird cases, so we'll tolerate it at the expense of
		// a not-so-helpful error message.
		fmt.Fprintf(buf, "  on %s line %d:\n  (source code not available)\n", diag.Range.Filename, diag.Range.Start.Line)
	} else {
		snippet := diag.Snippet
		code := snippet.Code

		var contextStr string
		if snippet.Context != "" {
			contextStr = fmt.Sprintf(", in %s", snippet.Context)
		}
		fmt.Fprintf(buf, "  on %s line %d%s:\n", diag.Range.Filename, diag.Range.Start.Line, contextStr)

		// Split the snippet and render the highlighted section with underlines
		start := int(snippet.HighlightStartOffset)
		end := int(snippet.HighlightEndOffset)

		// Only buggy diagnostics can have an end range before the start, but
		// we need to ensure we don't crash here if that happens.
		if end < start {
			end = start + 1
			if end > len(code) {
				end = len(code)
			}
		}

		// If either start or end is out of range for the code buffer then
		// we'll cap them at the bounds just to avoid a panic, although
		// this would happen only if there's a bug in the code generating
		// the snippet objects.
		if start < 0 {
			start = 0
		} else if start > len(code) {
			start = len(code)
		}
		if end < 0 {
			end = 0
		} else if end > len(code) {
			end = len(code)
		}

		before, highlight, after := code[0:start], code[start:end], code[end:]
		code = fmt.Sprintf(color.Color("%s[underline]%s[reset]%s"), before, highlight, after)

		// Split the snippet into lines and render one at a time
		lines := strings.Split(code, "\n")
		for i, line := range lines {
			fmt.Fprintf(
				buf, "%4d: %s\n",
				int(snippet.StartLine)+i,
				line,
			)
		}

		if len(snippet.Values) > 0 {
			// The diagnostic may also have information about the dynamic
			// values of relevant variables at the point of evaluation.
			// This is particularly useful for expressions that get evaluated
			// multiple times with different values, such as blocks using
			// "count" and "for_each", or within "for" expressions.
			values := make([]*pb.Diagnostic_ExpressionValue, len(snippet.Values))
			copy(values, snippet.Values)
			sort.Slice(values, func(i, j int) bool {
				return values[i].Traversal < values[j].Traversal
			})

			fmt.Fprint(buf, color.Color("    [dark_gray]├────────────────[reset]\n"))
			for _, value := range values {
				fmt.Fprintf(buf, color.Color("    [dark_gray]│[reset] [bold]%s[reset] %s\n"), value.Traversal, value.Statement)
			}
		}
	}

	buf.WriteByte('\n')
}

func hclRangeToProtoRange(rng hcl.Range) *pb.Range {
	return &pb.Range{
		Filename: rng.Filename,
		Start: &pb.Range_Pos{
			Line:   int64(rng.Start.Line),
			Byte:   int64(rng.Start.Byte),
			Column: int64(rng.Start.Column),
		},
		End: &pb.Range_Pos{
			Line:   int64(rng.End.Line),
			Byte:   int64(rng.End.Byte),
			Column: int64(rng.End.Column),
		},
	}
}

func parseRange(src []byte, rng hcl.Range) (*hcl.File, int) {
	filename := rng.Filename
	offset := rng.Start.Byte

	// We need to re-parse here to get a *hcl.File we can interrogate. This
	// is not awesome since we presumably already parsed the file earlier too,
	// but this re-parsing is architecturally simpler than retaining all of
	// the hcl.File objects and we only do this in the case of an error anyway
	// so the overhead here is not a big problem.
	parser := hclparse.NewParser()
	var file *hcl.File

	// Ignore diagnostics here as there is nothing we can do with them.
	file, _ = parser.ParseHCL(src, filename)

	return file, offset
}

// compactValueStr produces a compact, single-line summary of a given value
// that is suitable for display in the UI.
//
// For primitives it returns a full representation, while for more complex
// types it instead summarizes the type, size, etc to produce something
// that is hopefully still somewhat useful but not as verbose as a rendering
// of the entire data structure.
func compactValueStr(val cty.Value) string {
	// This is a specialized subset of value rendering tailored to producing
	// helpful but concise messages in diagnostics. It is not comprehensive
	// nor intended to be used for other purposes.

	ty := val.Type()
	switch {
	case val.IsNull():
		return "null"
	case !val.IsKnown():
		// Should never happen here because we should filter before we get
		// in here, but we'll do something reasonable rather than panic.
		return "(not yet known)"
	case ty == cty.Bool:
		if val.True() {
			return "true"
		}
		return "false"
	case ty == cty.Number:
		bf := val.AsBigFloat()
		return bf.Text('g', 10)
	case ty == cty.String:
		// Go string syntax is not exactly the same as HCL native string syntax,
		// but we'll accept the minor edge-cases where this is different here
		// for now, just to get something reasonable here.
		return fmt.Sprintf("%q", val.AsString())
	case ty.IsCollectionType() || ty.IsTupleType():
		l := val.LengthInt()
		switch l {
		case 0:
			return "empty " + ty.FriendlyName()
		case 1:
			return ty.FriendlyName() + " with 1 element"
		default:
			return fmt.Sprintf("%s with %d elements", ty.FriendlyName(), l)
		}
	case ty.IsObjectType():
		atys := ty.AttributeTypes()
		l := len(atys)
		switch l {
		case 0:
			return "object with no attributes"
		case 1:
			var name string
			for k := range atys {
				name = k
			}
			return fmt.Sprintf("object with 1 attribute %q", name)
		default:
			return fmt.Sprintf("object with %d attributes", l)
		}
	default:
		return ty.FriendlyName()
	}
}

// traversalStr produces a representation of an HCL traversal that is compact,
// resembles HCL native syntax, and is suitable for display in the UI.
func traversalStr(traversal hcl.Traversal) string {
	// This is a specialized subset of traversal rendering tailored to
	// producing helpful contextual messages in diagnostics. It is not
	// comprehensive nor intended to be used for other purposes.

	var buf bytes.Buffer
	for _, step := range traversal {
		switch tStep := step.(type) {
		case hcl.TraverseRoot:
			buf.WriteString(tStep.Name)
		case hcl.TraverseAttr:
			buf.WriteByte('.')
			buf.WriteString(tStep.Name)
		case hcl.TraverseIndex:
			buf.WriteByte('[')
			if keyTy := tStep.Key.Type(); keyTy.IsPrimitiveType() {
				buf.WriteString(compactValueStr(tStep.Key))
			} else {
				// We'll just use a placeholder for more complex values,
				// since otherwise our result could grow ridiculously long.
				buf.WriteString("...")
			}
			buf.WriteByte(']')
		}
	}
	return buf.String()
}