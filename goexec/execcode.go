package goexec

import (
	"fmt"
	"github.com/janpfeifer/gonb/kernel"
	"github.com/pkg/errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
)

// ExecuteCell takes the contents of a cell, parses it, merges new declarations with the ones
// from previous definitions, render a final main.go code with the whole content,
// compiles and runs it.
func (s *State) ExecuteCell(msg kernel.Message, lines []string, skipLines map[int]bool) error {
	// Find declarations on unchanged cell contents.
	_, err := s.createGoFileFromLines(s.MainPath(), lines, skipLines, NoCursor)
	if err != nil {
		return errors.WithMessagef(err, "in goexec.ExecuteCell()")
	}
	newDecls := NewDeclarations()
	if err = s.ParseImportsFromMainGo(msg, NoCursor, newDecls); err != nil {
		return errors.WithMessagef(err, "in goexec.ExecuteCell() while parsing cell")
	}

	// Checks whether there is a "main" function defined in the code.
	mainDecl, hasMain := newDecls.Functions["main"]
	if hasMain {
		// Remove "main" from newDecls: this should not be stored from one cell execution from
		// another.
		delete(newDecls.Functions, "main")
	} else {
		// Declare a stub main function, just so we can try to compile the final code.
		mainDecl = &Function{Key: "main", Name: "main", Definition: "func main() { flag.Parse() }"}
	}
	_ = mainDecl

	// Merge cell declarations with a copy of the current state: we don't want to commit the new
	// declarations until they compile successfully.
	tmpDecls := s.Decls.Copy()
	tmpDecls.MergeFrom(newDecls)

	// Render declarations to main.go.
	if _, err = s.createMainFromDecls(tmpDecls, mainDecl); err != nil {
		return errors.WithMessagef(err, "in goexec.ExecuteCell() while generating main.go with all declarations")
	}
	// Run goimports (or the code that implements it)
	if err = s.GoImports(msg); err != nil {
		return errors.WithMessagef(err, "goimports failed")
	}

	// And then compile it.
	if err := s.Compile(msg); err != nil {
		return err
	}

	// Compilation successful: save merged declarations into current State.
	s.Decls = tmpDecls

	// Execute compiled code.
	return s.Execute(msg)
}

func (s *State) BinaryPath() string {
	return path.Join(s.TempDir, s.Package)
}

func (s *State) MainPath() string {
	return path.Join(s.TempDir, "main.go")
}

func (s *State) Execute(msg kernel.Message) error {
	return kernel.PipeExecToJupyter(msg, "", s.BinaryPath(), s.Args...)
}

// Compile compiles the currently generate go files in State.TempDir to a binary named State.Package.
//
// If errors in compilation happen, linesPos is used to adjust line numbers to their content in the
// current cell.
func (s *State) Compile(msg kernel.Message) error {
	cmd := exec.Command("go", "build", "-o", s.BinaryPath())
	cmd.Dir = s.TempDir
	var output []byte
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.DisplayErrorWithContext(msg, string(output))
		return errors.Wrapf(err, "failed to run %q", cmd.String())
	}
	return nil
}

// GoImports execute `goimports` which adds imports to non-declared imports automatically.
// It also runs "go get" to download any missing dependencies.
func (s *State) GoImports(msg kernel.Message) error {
	goimportsPath, err := exec.LookPath("goimports")
	if err != nil {
		_ = kernel.PublishWriteStream(msg, kernel.StreamStderr, `
Program goimports is not installed. It is used to automatically import
missing standard packages, and is a standard Go toolkit package. You
can install it from the notebook with:

!go install golang.org/x/tools/cmd/goimports@latest

`)
		return errors.WithMessagef(err, "while trying to run goimports\n")
	}
	cmd := exec.Command(goimportsPath, "-w", s.MainPath())
	cmd.Dir = s.TempDir
	var output []byte
	output, err = cmd.CombinedOutput()
	if err != nil {
		s.DisplayErrorWithContext(msg, string(output)+"\n"+err.Error())
		return errors.Wrapf(err, "failed to run %q", cmd.String())
	}

	// Download missing dependencies.
	if !s.AutoGet {
		return nil
	}
	cmd = exec.Command("go", "get")
	cmd.Dir = s.TempDir
	output, err = cmd.CombinedOutput()
	if err != nil {
		s.DisplayErrorWithContext(msg, string(output)+"\n"+err.Error())
		return errors.Wrapf(err, "failed to run %q", cmd.String())
	}
	return nil
}

func (s *State) writeLinesToFile(filePath string, lines <-chan string) (err error) {
	var f *os.File
	f, err = os.Create(filePath)
	if err != nil {
		return errors.Wrapf(err, "creating %q", filePath)
	}
	defer func() {
		newErr := f.Close()
		if newErr != nil && err == nil {
			err = errors.Wrapf(newErr, "closing %q", filePath)
		}
	}()
	for line := range lines {
		if err != nil {
			// If there was an error keep on reading to the end of channel, discarding the input.
			continue
		}
		_, err = fmt.Fprintf(f, "%s\n", line)
		if err != nil {
			err = errors.Wrapf(err, "writing to %q", filePath)
		}
	}
	return err
}

// createGoFileFromLines implements CreateMainGo with no extra functionality (like auto-import).
func (s *State) createGoFileFromLines(filePath string, lines []string, skipLines map[int]bool, cursorInCell Cursor) (cursorInFile Cursor, err error) {
	linesChan := make(chan string, 1)

	cursorInFile = cursorInCell
	lineInFile := int32(0)
	go func() {
		defer close(linesChan)
		// addLine checks for the new cursorInFile position.
		addLine := func(line string, lineInCell int32, deltaColumn int32) {
			linesChan <- line
			lineInFile++

			if !cursorInCell.HasCursor() || lineInCell == NoCursorLine {
				return
			}
			if lineInCell == cursorInCell.Line {
				cursorInFile.Line = lineInFile - 1 // -1 because we already incremented lineInFile above.
				cursorInFile.Col = cursorInCell.Col + deltaColumn
				var modLine string
				if cursorInFile.Col < int32(len(line)) {
					modLine = line[:cursorInFile.Col] + "*" + line[cursorInFile.Col:]
				} else {
					modLine = line + "*"
				}
				log.Printf("Cursor in parse file line %d (cell line %d): %s", cursorInFile.Line, lineInCell, modLine)
			}
		}
		addEmptyLine := func() {
			addLine("", NoCursorLine, 0)
		}

		// Insert package.
		addLine("package main", NoCursorLine, 0)
		addEmptyLine()

		var createdFuncMain bool
		for ii, line := range lines {
			line = strings.TrimRight(line, " ")
			if line == "%main" || line == "%%" {
				addEmptyLine()
				addLine("func main() {", NoCursorLine, 0)
				addLine("\tflag.Parse()", NoCursorLine, 0)
				createdFuncMain = true
				continue
			}
			if skipLines[ii] {
				continue
			}
			if createdFuncMain {
				// Indent following lines.
				line = "\t" + line
				addLine(line, int32(ii), 1)
			} else {
				addLine(line, int32(ii), 0)
			}
		}
		if createdFuncMain {
			addLine("}", NoCursorLine, 0)
		}
	}()

	// Pipe linesChan to main.go file.
	err = s.writeLinesToFile(filePath, linesChan)

	// Check for any error only at the end.
	if err != nil {
		return NoCursor, err
	}
	return
}

func (s *State) createMainFromDecls(decls *Declarations, mainDecl *Function) (cursor Cursor, err error) {
	cursor = NoCursor

	var f *os.File
	f, err = os.Create(s.MainPath())
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = errors.Wrapf(err, "creating main.go")
			return
		}
		err = f.Close()
		if err != nil {
			err = errors.Wrapf(err, "closing main.go")
		}
	}()

	lineNum := 0
	w := func(format string, args ...any) {
		if err != nil {
			return
		}
		strBuf := fmt.Sprintf(format, args...)
		lineNum += countLines(strBuf)
		_, err = fmt.Fprint(f, strBuf)
	}

	w("package main\n\n")
	if err != nil {
		return
	}

	update := func(fn func(lineNum int, w io.Writer) (int, Cursor, error), name string) bool {
		var newCursor Cursor
		var newLineNum int
		newLineNum, newCursor, err = fn(lineNum, f)
		if newLineNum != lineNum {
			//log.Printf("Block %q: lines (%d - %d)", name, lineNum, newLineNum)
			lineNum = newLineNum
		}
		if err != nil {
			err = errors.WithMessagef(err, "in block %q", name)
			return true
		}
		if newCursor.HasCursor() {
			cursor = newCursor
			//log.Printf("Cursor found in %q: %v", name, cursor)
		}
		return false
	}

	if update(decls.RenderImports, "imports") {
		return
	}
	if update(decls.RenderTypes, "types") {
		return
	}
	if update(decls.RenderConstants, "constants") {
		return
	}
	if update(decls.RenderVariables, "variables") {
		return
	}
	if update(decls.RenderFunctions, "functions") {
		return
	}
	w("\n")
	if mainDecl.HasCursor() {
		cursor = mainDecl.Cursor
		cursor.Line += int32(lineNum)
		//log.Printf("Cursor in \"main\": %v", cursor)
	}
	w("%s\n", mainDecl.Definition)
	return
}
