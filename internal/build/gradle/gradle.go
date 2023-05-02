package gradle

import (
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/pkg/errors"

	"code-intelligence.com/cifuzz/internal/build"
	"code-intelligence.com/cifuzz/internal/cmdutils"
	"code-intelligence.com/cifuzz/internal/config"
	"code-intelligence.com/cifuzz/pkg/log"
	"code-intelligence.com/cifuzz/pkg/messaging"
	"code-intelligence.com/cifuzz/pkg/runfiles"
	"code-intelligence.com/cifuzz/util/fileutil"
)

var (
	classpathRegex = regexp.MustCompile("(?m)^cifuzz.test.classpath=(?P<classpath>.*)$")
	buildDirRegex  = regexp.MustCompile("(?m)^cifuzz.buildDir=(?P<buildDir>.*)$")
)

func FindGradleWrapper(projectDir string) (string, error) {
	wrapper := "gradlew"
	if runtime.GOOS == "windows" {
		wrapper = "gradlew.bat"
	}

	return fileutil.SearchFileBackwards(projectDir, wrapper)
}

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

func (b *Builder) Build(targetClass string) (*build.Result, error) {
	gradleBuildLanguage, err := config.DetermineGradleBuildLanguage(b.ProjectDir)
	if err != nil {
		return nil, err
	}

	version, err := b.GradlePluginVersion()
	if err != nil {
		log.Warn("No cifuzz gradle plugin found")
		log.Print(messaging.Instructions(string(gradleBuildLanguage)))
		return nil, cmdutils.WrapSilentError(err)
	}
	log.Debugf("Found gradle plugin version: %s", version)

	var flags []string
	if b.Parallel.Enabled {
		flags = append(flags, "--parallel")
	}

	deps, err := b.getDependencies()
	if err != nil {
		return nil, err
	}
	seedCorpus := cmdutils.JazzerSeedCorpus(targetClass, b.ProjectDir)
	generatedCorpus := cmdutils.JazzerGeneratedCorpus(targetClass, b.ProjectDir)

	buildDir, err := GetBuildDirectory(b.ProjectDir)
	if err != nil {
		return nil, err
	}
	result := &build.Result{
		Name:            targetClass,
		BuildDir:        buildDir,
		ProjectDir:      b.ProjectDir,
		GeneratedCorpus: generatedCorpus,
		SeedCorpus:      seedCorpus,
		RuntimeDeps:     deps,
	}

	return result, nil
}

func (b *Builder) GradlePluginVersion() (string, error) {
	cmd, err := buildGradleCommand(b.ProjectDir, []string{"cifuzzPrintPluginVersion", "-q"})
	if err != nil {
		return "", errors.WithStack(err)
	}
	log.Debugf("Command: %s", cmd.String())
	output, err := cmd.Output()
	if err != nil {
		return "", errors.WithStack(err)
	}

	return strings.TrimPrefix(string(output), "cifuzz.plugin.version="), nil
}

func (b *Builder) getDependencies() ([]string, error) {
	cmd, err := buildGradleCommand(b.ProjectDir, []string{"cifuzzPrintTestClasspath", "-q"})
	if err != nil {
		return nil, err
	}
	log.Debugf("Command: %s", cmd.String())
	output, err := cmd.Output()
	if err != nil {
		return nil, cmdutils.WrapExecError(errors.WithStack(err), cmd)
	}
	classpath := classpathRegex.FindStringSubmatch(string(output))
	deps := strings.Split(strings.TrimSpace(classpath[1]), string(os.PathListSeparator))

	return deps, nil
}

// GetGradleCommand returns the name of the gradle command.
// The gradle wrapper is preferred to use and gradle
// acts as a fallback command.
func GetGradleCommand(projectDir string) (string, error) {
	wrapper, err := FindGradleWrapper(projectDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if wrapper != "" {
		return wrapper, nil
	}

	gradleCmd, err := runfiles.Finder.GradlePath()
	if err != nil {
		return "", errors.WithStack(err)
	}
	return gradleCmd, nil
}

func buildGradleCommand(projectDir string, args []string) (*exec.Cmd, error) {
	gradleCmd, err := GetGradleCommand(projectDir)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(gradleCmd, args...)
	cmd.Dir = projectDir

	return cmd, nil
}

func GetBuildDirectory(projectDir string) (string, error) {
	cmd, err := buildGradleCommand(projectDir, []string{"cifuzzPrintBuildDir", "-q"})
	if err != nil {
		return "", nil
	}

	log.Debugf("Command: %s", cmd.String())
	output, err := cmd.Output()
	if err != nil {
		return "", cmdutils.WrapExecError(errors.WithStack(err), cmd)
	}
	result := buildDirRegex.FindStringSubmatch(string(output))
	if result == nil {
		return "", errors.New("Unable to parse gradle build directory from init script.")
	}
	buildDir := strings.TrimSpace(result[1])

	return buildDir, nil
}
