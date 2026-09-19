package classifier

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	"github.com/spencercnorton/bitagent/internal/keywords"
	"github.com/spencercnorton/bitagent/internal/model"
	"github.com/spencercnorton/bitagent/internal/protobuf"
)

// eachKeywordRegex compiles one regex per keyword, deduplicated so that a
// repeated keyword cannot inflate a distinct-hit count.
func eachKeywordRegex(kws []string) ([]string, error) {
	seen := make(map[string]struct{}, len(kws))
	out := make([]string, 0, len(kws))

	for _, kw := range kws {
		if _, ok := seen[kw]; ok {
			continue
		}

		seen[kw] = struct{}{}

		r, err := keywords.NewRegexFromKeywords(kw)
		if err != nil {
			return nil, err
		}

		out = append(out, r.String())
	}

	return out, nil
}

func celEnvOption(src Source, ctx *compilerContext) error {
	options := []cel.EnvOption{
		cel.StdLib(),
		Lists(),
		cel.EagerlyValidateDeclarations(true),
		cel.ExtendedValidations(),
		ext.Strings(ext.StringsValidateFormatCalls(true)),
		cel.Types(&protobuf.Torrent{}, &protobuf.Classification{}),
		cel.Variable("torrent", cel.ObjectType("bitmagnet.Torrent")),
		cel.Variable("result", cel.ObjectType("bitmagnet.Classification")),
	}
	// `flags` is masquerading as a map of strings to regexes, but it's actually individual variables defined
	// with a dot in the name, along with a placeholder map of strings to nulls. This achieves correct compile-time
	// checking with acceptable error messages.
	for name, tp := range src.FlagDefinitions {
		options = append(
			options,
			cel.Variable("flags."+name, tp.celType()),
		)
	}

	options = append(
		options,
		cel.Constant("flags", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
	)
	// `keywords`, `extensions` etc use a similar trick.
	for group, kws := range src.Keywords {
		r, err := keywords.NewRegexFromKeywords(kws...)
		if err != nil {
			return err
		}

		options = append(
			options,
			cel.Constant("keywords."+group, cel.StringType, types.String(r.String())),
		)

		// `keywordList.<group>` is the same group as one regex per keyword, so a
		// workflow can require N *distinct* keywords to hit instead of just one:
		//
		//   keywordList.xxx_weak.filter(k, text.matches(k)).size() >= 2
		//
		// The combined `keywords.<group>` regex cannot express that. Counting
		// matches on it undercounts, because adjacent hits share the separator
		// the boundary group consumes ("Big.Ass.Tits" finds one match, not two).
		each, err := eachKeywordRegex(kws)
		if err != nil {
			return err
		}

		options = append(
			options,
			cel.Constant(
				"keywordList."+group,
				cel.ListType(cel.StringType),
				types.NewStringList(types.DefaultTypeAdapter, each),
			),
		)
	}

	options = append(
		options,
		cel.Constant("keywords", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
		cel.Constant("keywordList", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
	)
	for group, extensions := range src.Extensions {
		options = append(
			options,
			cel.Constant(
				"extensions."+group,
				cel.ListType(cel.StringType),
				types.NewStringList(types.DefaultTypeAdapter, extensions),
			),
		)
	}

	options = append(
		options,
		cel.Constant("extensions", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
	)
	options = append(
		options,
		cel.Constant("fileType.unknown", cel.IntType, types.Int(protobuf.Torrent_File_unknown)),
	)

	for _, ft := range model.FileTypeValues() {
		options = append(
			options,
			cel.Constant(
				fmt.Sprintf("fileType.%s", ft.String()),
				cel.IntType,
				types.Int(protobuf.NewFileType(model.NullFileType{Valid: true, FileType: ft})),
			),
		)
	}

	options = append(
		options,
		cel.Constant("fileType", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
	)
	options = append(
		options,
		cel.Constant("contentType.unknown", cel.IntType, types.Int(protobuf.Classification_unknown)),
	)

	for _, ct := range model.ContentTypeValues() {
		options = append(
			options,
			cel.Constant(
				fmt.Sprintf("contentType.%s", ct.String()),
				cel.IntType,
				types.Int(protobuf.NewContentType(model.NullContentType{Valid: true, ContentType: ct})),
			),
		)
	}

	options = append(
		options,
		cel.Constant("contentType", cel.MapType(cel.StringType, cel.NullType), types.NullValue),
	)
	options = append(
		options,
		cel.Constant("kb", cel.IntType, types.Int(1_000)),
	)
	options = append(
		options,
		cel.Constant("mb", cel.IntType, types.Int(1_000_000)),
	)
	options = append(
		options,
		cel.Constant("gb", cel.IntType, types.Int(1_000_000_000)),
	)

	env, err := cel.NewCustomEnv(options...)
	if err != nil {
		return err
	}

	ctx.celEnv = env

	return nil
}
