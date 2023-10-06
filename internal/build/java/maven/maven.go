package maven

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/pkg/errors"

	"code-intelligence.com/cifuzz/internal/build"
	"code-intelligence.com/cifuzz/internal/cmdutils"
	"code-intelligence.com/cifuzz/pkg/log"
	"code-intelligence.com/cifuzz/pkg/runfiles"
	"code-intelligence.com/cifuzz/util/fileutil"
)

var (
	classpathRegex         = regexp.MustCompile("(?m)^cifuzz.test.classpath=(?P<classpath>.*)$")
	buildDirRegex          = regexp.MustCompile("(?m)^cifuzz.buildDir=(?P<buildDir>.*)$")
	testSourceFoldersRegex = regexp.MustCompile("(?m)^cifuzz.test.source-folders=(?P<testSourceFolders>.*)$")
	mainSourceFoldersRegex = regexp.MustCompile("(?m)^cifuzz.main.source-folders=(?P<mainSourceFolders>.*)$")
)

type ParallelOptions struct {
	Enabled bool
	NumJobs uint
}

type BuilderOptions struct {
	ProjectDir string
	Parallel   ParallelOptions
	Stdout     io.Writer
	Stderr     io.Writer
}

func (opts *BuilderOptions) Validate() error {
	// Check that the project dir is set
	if opts.ProjectDir == "" {
		return errors.New("ProjectDir is not set")
	}
	// Check that the project dir exists and can be accessed
	_, err := os.Stat(opts.ProjectDir)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

type Builder struct {
	*BuilderOptions
}

func NewBuilder(opts *BuilderOptions) (*Builder, error) {
	err := opts.Validate()
	if err != nil {
		return nil, err
	}

	b := &Builder{BuilderOptions: opts}

	return b, err
}

func (b *Builder) Build() (*build.BuildResult, error) {
	deps, err := GetDependencies(b.ProjectDir, b.Parallel)
	if err != nil {
		return nil, err
	}

	result := &build.BuildResult{
		// BuildDir is not used by Jazzer
		BuildDir:    "",
		RuntimeDeps: deps,
	}

	return result, nil
}

func GetDependencies(projectDir string, parallel ParallelOptions) ([]string, error) {
	var flags []string
	if parallel.Enabled {
		flags = append(flags, "-T")
		if parallel.NumJobs != 0 {
			flags = append(flags, fmt.Sprint(parallel.NumJobs))
		} else {
			// Use one thread per cpu core
			flags = append(flags, "1C")
		}
	}

	args := append(flags, "test-compile", "-DcifuzzPrintTestClasspath")
	cmd := runMaven(projectDir, args)
	output, err := cmd.Output()
	if err != nil {
		return nil, cmdutils.WrapExecError(errors.WithStack(err), cmd)
	}

	classpath := classpathRegex.FindStringSubmatch(string(output))
	deps := strings.Split(strings.TrimSpace(classpath[1]), string(os.PathListSeparator))

	// Add jacoco cli and java agent JAR paths
	cliJarPath, err := runfiles.Finder.JacocoCLIJarPath()
	if err != nil {
		return nil, err
	}
	agentJarPath, err := runfiles.Finder.JacocoAgentJarPath()
	if err != nil {
		return nil, err
	}
	deps = append(deps, cliJarPath, agentJarPath)
	return deps, nil
}

func runMaven(projectDir string, args []string) *exec.Cmd {
	// remove color and transfer progress from output
	args = append(args, "-B", "--no-transfer-progress")
	cmd := exec.Command("mvn", args...) // TODO find ./mvnw if available (unify with MavenRunner in coverage.go)
	cmd.Dir = projectDir

	log.Debugf("Working directory: %s", cmd.Dir)
	log.Debugf("Command: %s", cmd.String())

	return cmd
}

func GetBuildDirectory(projectDir string) (string, error) {
	cmd := runMaven(projectDir, []string{"validate", "-q", "-DcifuzzPrintBuildDir"})
	output, err := cmd.Output()
	if err != nil {
		return "", cmdutils.WrapExecError(errors.WithStack(err), cmd)
	}

	result := buildDirRegex.FindStringSubmatch(string(output))
	if result == nil {
		return "", errors.New("Unable to parse maven build directory.")
	}
	buildDir := strings.TrimSpace(result[1])

	return buildDir, nil
}

// GetTestDir returns the value of <testSourceDirectory> for the fuzz project
// (which may be one of the sub-modules in a multi-project)
func GetTestDir(projectDir string) (string, error) {
	cmd := runMaven(projectDir, []string{"validate", "-q", "-DcifuzzPrintTestSourceFolders"})
	output, err := cmd.Output()
	if err != nil {
		return "", cmdutils.WrapExecError(errors.WithStack(err), cmd)
	}

	result := testSourceFoldersRegex.FindStringSubmatch(string(output))
	if result == nil {
		return "", errors.New("Unable to parse maven test sources.")
	}
	testDir := strings.TrimSpace(result[1])
	log.Debugf("Found Maven test source at: %s", testDir)

	exists, err := fileutil.Exists(testDir)
	if err != nil {
		return "", err
	}
	if exists {
		return testDir, nil
	}
	log.Debugf("Ignoring Maven test source directory %s: directory does not exist", testDir)

	return "", nil
}

// GetSourceDir returns the value of <sourceDirectory> for the fuzz project
// (which may be one of the sub-modules in a multi-project)
func GetSourceDir(projectDir string) (string, error) {
	cmd := runMaven(projectDir, []string{"validate", "-q", "-DcifuzzPrintMainSourceFolders"})
	output, err := cmd.Output()
	if err != nil {
		return "", errors.WithMessagef(err, "Failed to get source directory of project")
	}

	result := mainSourceFoldersRegex.FindStringSubmatch(string(output))
	if result == nil {
		return "", errors.New("Unable to parse maven main sources.")
	}
	sourceDir := strings.TrimSpace(result[1])
	log.Debugf("Found Maven source at: %s", sourceDir)

	exists, err := fileutil.Exists(sourceDir)
	if err != nil {
		return "", errors.WithMessagef(err, "Error checking if Maven source directory %s exists", sourceDir)
	}
	if exists {
		return sourceDir, nil
	}
	log.Debugf("Ignoring Maven source directory %s: directory does not exist", sourceDir)

	return "", nil
}
