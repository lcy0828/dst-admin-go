package luaconfig

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/yuin/gopher-lua/ast"
	"github.com/yuin/gopher-lua/parse"
)

const (
	DefaultMaxDepth   = 100
	DefaultMaxEntries = 100000
)

var (
	ErrFileTooLarge        = errors.New("Lua configuration file exceeds the size limit")
	ErrUnsupportedLuaValue = errors.New("Lua configuration contains an unsupported value")
	kleiPersistentHeader   = regexp.MustCompile(`^\s*KLEI\s+\d+[ \t]*(\r?\n|[ \t]+)`)
)

type NormalizeOptions struct {
	MaxBytes       int64
	AllowBareTable bool
}

type NormalizedSource struct {
	Data               []byte
	StrippedBOM        bool
	StrippedKleiHeader bool
	WrappedReturn      bool
	RemovedLines       int
	Notices            []string
}

type SyntaxError struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *SyntaxError) Error() string {
	if e == nil {
		return ""
	}
	if e.Line > 0 {
		if e.Column > 0 {
			return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Column, e.Message)
		}
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
	}
	if e.File != "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return e.Message
}

func (e *SyntaxError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NormalizeSource cleans client-copied DST Lua files (UTF-8 BOM, CRLF,
// "KLEI     1" persistent headers, and optional bare "{ ... }" tables) while
// preserving comments, indentation, and line alignment wherever possible.
func NormalizeSource(raw []byte, opts NormalizeOptions) (NormalizedSource, error) {
	if opts.MaxBytes > 0 && int64(len(raw)) > opts.MaxBytes {
		return NormalizedSource{}, ErrFileTooLarge
	}
	result := NormalizedSource{}
	data := raw

	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		data = data[3:]
		result.StrippedBOM = true
	}

	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))

	if loc := kleiPersistentHeader.FindIndex(data); loc != nil {
		remainder := skipLeadingCommentsAndSpace(data[loc[1]:])
		if bytes.HasPrefix(remainder, []byte("return")) || bytes.HasPrefix(remainder, []byte("{")) {
			removedLines := bytes.Count(data[:loc[1]], []byte("\n"))
			data = data[loc[1]:]
			if removedLines > 0 {
				// Preserve trailing newline behavior cleanly without shifting content.
				data = bytes.TrimLeft(data, " \t")
			}
			result.StrippedKleiHeader = true
			result.RemovedLines = removedLines
			result.Notices = append(result.Notices, "stripped_klei_header")
		}
	}

	if opts.AllowBareTable {
		trimmedOffset := leadingCommentsAndSpaceOffset(data)
		if trimmedOffset < len(data) && data[trimmedOffset] == '{' {
			var combined bytes.Buffer
			combined.Grow(len(data) + len("return ") + 1)
			combined.Write(data[:trimmedOffset])
			combined.WriteString("return ")
			combined.Write(data[trimmedOffset:])
			data = combined.Bytes()
			result.WrappedReturn = true
			result.Notices = append(result.Notices, "wrapped_bare_table")
		}
	}

	if len(bytes.TrimSpace(data)) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		copied := make([]byte, len(data)+1)
		copy(copied, data)
		copied[len(data)] = '\n'
		data = copied
	} else {
		copied := make([]byte, len(data))
		copy(copied, data)
		data = copied
	}

	if opts.MaxBytes > 0 && int64(len(data)) > opts.MaxBytes {
		return NormalizedSource{}, ErrFileTooLarge
	}
	result.Data = data
	return result, nil
}

// ValidateNormalizedReturnTable validates NormalizedSource and maps AST line
// numbers back to the caller's original buffer when leading header lines were
// stripped.
func ValidateNormalizedReturnTable(normalized NormalizedSource, fileName string, maxBytes int64) error {
	err := ValidateLiteralReturnTable(normalized.Data, fileName, maxBytes)
	if err == nil || normalized.RemovedLines <= 0 {
		return err
	}
	var syntaxErr *SyntaxError
	if errors.As(err, &syntaxErr) && syntaxErr.Line > 0 {
		syntaxErr.Line += normalized.RemovedLines
	}
	return err
}

// ValidateLiteralReturnTable parses a Lua document at the AST level (never
// executing loops or calls) and ensures it returns a single literal table with
// no dynamic expressions or duplicate keys.
func ValidateLiteralReturnTable(data []byte, fileName string, maxBytes int64) error {
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return ErrFileTooLarge
	}
	chunk, err := parse.Parse(bytes.NewReader(data), fileName)
	if err != nil {
		var parseErr *parse.Error
		if errors.As(err, &parseErr) {
			msg := strings.TrimSpace(parseErr.Message)
			if msg == "" {
				msg = err.Error()
			}
			return &SyntaxError{
				File:    fileName,
				Line:    parseErr.Pos.Line,
				Column:  parseErr.Pos.Column,
				Message: msg,
				Cause:   err,
			}
		}
		return &SyntaxError{File: fileName, Message: err.Error(), Cause: err}
	}
	if len(chunk) != 1 {
		line := 1
		if len(chunk) > 1 {
			line = chunk[1].Line()
		}
		return &SyntaxError{
			File:    fileName,
			Line:    line,
			Message: fmt.Sprintf("%s must contain a single return table statement", fileName),
			Cause:   ErrUnsupportedLuaValue,
		}
	}
	statement, ok := chunk[0].(*ast.ReturnStmt)
	if !ok || len(statement.Exprs) != 1 {
		return &SyntaxError{
			File:    fileName,
			Line:    chunk[0].Line(),
			Message: fmt.Sprintf("%s must return one literal table", fileName),
			Cause:   ErrUnsupportedLuaValue,
		}
	}
	if _, ok := statement.Exprs[0].(*ast.TableExpr); !ok {
		return &SyntaxError{
			File:    fileName,
			Line:    statement.Exprs[0].Line(),
			Message: fmt.Sprintf("%s must return a literal table", fileName),
			Cause:   ErrUnsupportedLuaValue,
		}
	}
	count := 0
	return validateLiteralExpr(fileName, statement.Exprs[0], 0, &count)
}

func validateLiteralExpr(fileName string, expression ast.Expr, depth int, count *int) error {
	if depth > DefaultMaxDepth {
		return &SyntaxError{
			File:    fileName,
			Line:    expression.Line(),
			Message: fmt.Sprintf("table nesting exceeds %d levels", DefaultMaxDepth),
			Cause:   ErrUnsupportedLuaValue,
		}
	}
	switch value := expression.(type) {
	case *ast.StringExpr, *ast.NumberExpr, *ast.TrueExpr, *ast.FalseExpr, *ast.NilExpr:
		return nil
	case *ast.UnaryMinusOpExpr:
		if _, ok := value.Expr.(*ast.NumberExpr); ok {
			return nil
		}
	case *ast.TableExpr:
		keys := make(map[string]bool, len(value.Fields))
		arrayIndex := 0
		for _, field := range value.Fields {
			*count++
			if *count > DefaultMaxEntries {
				return &SyntaxError{
					File:    fileName,
					Line:    expression.Line(),
					Message: fmt.Sprintf("table exceeds %d entries", DefaultMaxEntries),
					Cause:   ErrUnsupportedLuaValue,
				}
			}
			key := ""
			line := expression.Line()
			if field.Value != nil && field.Value.Line() > 0 {
				line = field.Value.Line()
			}
			if field.Key == nil {
				arrayIndex++
				key = "n:" + strconv.Itoa(arrayIndex)
			} else {
				if field.Key.Line() > 0 {
					line = field.Key.Line()
				}
				var err error
				key, err = literalTableKey(fileName, field.Key)
				if err != nil {
					return err
				}
			}
			if keys[key] {
				displayKey := key
				if len(key) > 2 {
					displayKey = key[2:]
				}
				return &SyntaxError{
					File:    fileName,
					Line:    line,
					Message: fmt.Sprintf("duplicate Lua table key %q", displayKey),
					Cause:   ErrUnsupportedLuaValue,
				}
			}
			keys[key] = true
			if err := validateLiteralExpr(fileName, field.Value, depth+1, count); err != nil {
				return err
			}
		}
		return nil
	}
	return &SyntaxError{
		File:    fileName,
		Line:    expression.Line(),
		Message: "dynamic Lua expressions are not supported in configuration files",
		Cause:   ErrUnsupportedLuaValue,
	}
}

func literalTableKey(fileName string, expression ast.Expr) (string, error) {
	switch value := expression.(type) {
	case *ast.StringExpr:
		return "s:" + value.Value, nil
	case *ast.TrueExpr:
		return "b:true", nil
	case *ast.FalseExpr:
		return "b:false", nil
	case *ast.NumberExpr:
		number, err := strconv.ParseFloat(value.Value, 64)
		if err != nil {
			integer, intErr := strconv.ParseInt(value.Value, 0, 64)
			if intErr != nil {
				return "", &SyntaxError{File: fileName, Line: expression.Line(), Message: "invalid numeric table key", Cause: intErr}
			}
			number = float64(integer)
		}
		return "n:" + strconv.FormatFloat(number, 'g', -1, 64), nil
	case *ast.UnaryMinusOpExpr:
		key, err := literalTableKey(fileName, value.Expr)
		if err == nil && len(key) > 2 && key[:2] == "n:" {
			number, _ := strconv.ParseFloat(key[2:], 64)
			if number == 0 {
				return "n:0", nil
			}
			return "n:" + strconv.FormatFloat(-number, 'g', -1, 64), nil
		}
	}
	return "", &SyntaxError{
		File:    fileName,
		Line:    expression.Line(),
		Message: "dynamic Lua table keys are not supported",
		Cause:   ErrUnsupportedLuaValue,
	}
}

func skipLeadingCommentsAndSpace(data []byte) []byte {
	return data[leadingCommentsAndSpaceOffset(data):]
}

func leadingCommentsAndSpaceOffset(data []byte) int {
	i := 0
	for i < len(data) {
		for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n') {
			i++
		}
		if i+1 < len(data) && data[i] == '-' && data[i+1] == '-' {
			i += 2
			if i < len(data) && data[i] == '[' {
				eqCount := 0
				j := i + 1
				for j < len(data) && data[j] == '=' {
					eqCount++
					j++
				}
				if j < len(data) && data[j] == '[' {
					closing := "]" + strings.Repeat("=", eqCount) + "]"
					end := bytes.Index(data[j+1:], []byte(closing))
					if end >= 0 {
						i = j + 1 + end + len(closing)
						continue
					}
					return len(data)
				}
			}
			for i < len(data) && data[i] != '\n' {
				i++
			}
			continue
		}
		break
	}
	return i
}
