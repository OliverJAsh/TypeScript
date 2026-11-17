package compiler

// Spike for https://github.com/microsoft/TypeScript/issues/65313: content mappers for tagged
// template literals.
//
// A content mapper can declare "tags" in tsconfig. When a TypeScript or JavaScript file contains a tagged
// template whose tag is an identifier with one of those names, the compiler sends the text of the tagged
// template (from the tag to the closing backtick) to the mapper, with the tag in the request. The mapper
// returns a TypeScript expression and span mappings relative to that text. The compiler then makes a virtual
// copy of the file: the text outside the templates is copied verbatim, and each template is replaced with
// the mapper's expression. The virtual file uses the existing content-mapped source file machinery, so
// diagnostics and language service features map back to the original text.
//
// Limitations of the spike:
//   - Tags match by name only, before binding. A local variable named like a tag is also mapped.
//   - Templates nested in the substitutions of a mapped template are not mapped.
//   - JavaScript emit for a mapped file would use the virtual text, which is for type checking only. Thus
//     mapped files are not emitted. Emit from the original text needs more changes.
//   - Results are not cached: each new program sends the templates of new or changed files again.

import (
	"strings"

	"github.com/microsoft/TypeScript/tsc/internal/ast"
	"github.com/microsoft/TypeScript/tsc/internal/contentmapper"
	"github.com/microsoft/TypeScript/tsc/internal/core"
	"github.com/microsoft/TypeScript/tsc/internal/parser"
	"github.com/microsoft/TypeScript/tsc/internal/scanner"
	"github.com/microsoft/TypeScript/tsc/internal/spanmap"
)

// applyTagMappers returns a content-mapped copy of file in which each tagged template with a registered tag is
// replaced by its mapper's expression, or file itself if it has no such templates.
func (p *fileLoader) applyTagMappers(file *ast.SourceFile, opts ast.SourceFileParseOptions) *ast.SourceFile {
	if file == nil || file.IsDeclarationFile || file.IsContentMapped() {
		return file
	}
	tagMappers := p.opts.Config.TagMappers()
	if len(tagMappers) == 0 {
		return file
	}
	project := p.host.ContentMapperProject()
	if project == nil {
		return file
	}

	var templates []*ast.Node
	var visit ast.Visitor
	visit = func(node *ast.Node) bool {
		if node.Kind == ast.KindTaggedTemplateExpression {
			tag := node.AsTaggedTemplateExpression().Tag
			if ast.IsIdentifier(tag) && tagMappers[tag.Text()] != nil {
				templates = append(templates, node)
				return false
			}
		}
		return node.ForEachChild(visit)
	}
	file.ForEachChild(visit)
	if len(templates) == 0 {
		return file
	}

	text := file.Text()
	var virtual strings.Builder
	var segments []spanmap.Segment
	var mapperDiagnostics []*ast.Diagnostic
	var failures []*ast.Diagnostic
	identity := ""
	transformIdentity := ""
	copied := 0
	copyTo := func(end int) {
		if end > copied {
			start := virtual.Len()
			virtual.WriteString(text[copied:end])
			segments = append(segments, spanmap.Segment{
				VirtualStart:  core.TextPos(start),
				VirtualEnd:    core.TextPos(virtual.Len()),
				OriginalStart: core.TextPos(copied),
				OriginalEnd:   core.TextPos(end),
				Kind:          spanmap.KindVerbatim,
				Features:      spanmap.FeatureAll,
			})
		}
		copied = end
	}

	for _, node := range templates {
		tag := node.AsTaggedTemplateExpression().Tag.Text()
		mapper := tagMappers[tag]
		if p.contentMapperUnavailable(mapper) {
			continue
		}
		start := scanner.SkipTrivia(text, node.Pos())
		end := node.End()
		content := text[start:end]
		result, err := project.Transform(mapper, contentmapper.Request{FileName: opts.FileName, Content: content, Tag: tag})
		if err == nil {
			if result.Mappings == nil {
				err = contentmapper.NewTransformError(contentmapper.TransformErrorKindMappings, nil)
			} else if problem := result.Mappings.Validate(result.Text, content); problem != nil {
				err = problem
			}
		}
		if err != nil {
			// Keep the original template text. It is copied with the next verbatim segment.
			if p.recordContentMapperFailure(mapper, mapper.DiagnosticName()) {
				diagnostic := contentMapperTransformDiagnostic(nil, mapper.DiagnosticName(), err)
				failures = append(failures, diagnostic)
			}
			continue
		}
		identity = mapper.Identity()
		transformIdentity = p.getContentMapperTransformIdentity(mapper)
		copyTo(start)
		base := virtual.Len()
		for _, segment := range result.Mappings.Segments() {
			segment.VirtualStart += core.TextPos(base)
			segment.VirtualEnd += core.TextPos(base)
			segment.OriginalStart += core.TextPos(start)
			segment.OriginalEnd += core.TextPos(start)
			segments = append(segments, segment)
		}
		for _, d := range result.Diagnostics {
			mapperDiagnostics = append(mapperDiagnostics, ast.NewExternalDiagnostic(
				nil,
				core.NewTextRange(d.Pos()+start, d.End()+start),
				d.Source(),
				d.Category(),
				d.Code(),
				d.MessageText(),
			))
		}
		virtual.WriteString(result.Text)
		copied = end
	}
	if identity == "" {
		// Every transform failed. Report the failures on the original file.
		for _, diagnostic := range failures {
			diagnostic.SetFile(file)
		}
		if len(failures) > 0 {
			file = parser.ParseSourceFile(opts, text, file.ScriptKind)
			file.SetDiagnostics(append(file.Diagnostics(), failures...))
		}
		return file
	}
	copyTo(len(text))

	mapped := parser.ParseSourceFile(opts, virtual.String(), file.ScriptKind)
	extra := append(mapperDiagnostics, failures...)
	for _, diagnostic := range extra {
		diagnostic.SetFile(mapped)
	}
	mapped.SetDiagnostics(append(mapped.Diagnostics(), extra...))
	mapped.SetContentMapperInfo(ast.ContentMapperSourceFileInfo{
		ContentMapper:     identity,
		TransformIdentity: transformIdentity,
		ParseOptions:      opts,
		VirtualFileName:   opts.FileName,
		OriginalText:      text,
		SpanMap:           spanmap.New(segments),
	})
	return mapped
}
