/*
Package config is the YAML parser of the task file for Dunner.

For more information on how to write a task file for Dunner, please refer to the
following link of an article on Dunner repository's Wiki:
https://github.com/leopardslab/dunner/dunner/wiki/User-Guide#how-to-write-a-dunner-file

Usage

You can use the library by creating a dunner task file. For example,
	# .dunner.yaml
	prepare:
	  - image: node
		commands:
		  - ["node", "--version"]
	  - image: node
		commands:
		  - ["npm", "install"]
	  - image: mvn
		commands:
		  - ["mvn", "package"]

Use `GetConfigs` method to parse the dunner task file, and `ParseEnvs` method to parse environment variables file, or
the host environment variables. The environment variables are used by invoking in the task file using backticks(`$var`).
*/
package config

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/mount"
	"github.com/go-playground/locales/en"
	ut "github.com/go-playground/universal-translator"
	"github.com/joho/godotenv"
	"github.com/leopardslab/dunner/internal"
	"github.com/leopardslab/dunner/internal/logger"
	"github.com/leopardslab/dunner/internal/util"
	"github.com/leopardslab/dunner/pkg/docker"
	"github.com/spf13/viper"
	validator "gopkg.in/go-playground/validator.v9"
	en_translations "gopkg.in/go-playground/validator.v9/translations/en"
	yaml "gopkg.in/yaml.v2"
)

var log = logger.Log
var dotEnv map[string]string
var hostDirpattern = "`\\$(?P<name>[^`]+)`"
var hostDirRegex = regexp.MustCompile(hostDirpattern)

var (
	uni                     *ut.UniversalTranslator
	govalidator             *validator.Validate
	trans                   ut.Translator
	defaultPermissionMode   = "r"
	validDirPermissionModes = []string{defaultPermissionMode, "wr", "rw", "w"}
)

type contextKey string

var configsKey = contextKey("dunnerConfigs")

type customValidation struct {
	tag          string
	translation  string
	validationFn func(context.Context, validator.FieldLevel) bool
}

var customValidations = []customValidation{
	{
		tag:          "mountdir",
		translation:  "mount directory '{0}' is invalid. Check format is '<valid_src_dir>:<valid_dest_dir>:<optional_mode>' and has right permission level",
		validationFn: ValidateMountDir,
	},
	{
		tag:          "follow_exist",
		translation:  "follow task '{0}' does not exist",
		validationFn: ValidateFollowTaskPresent,
	},
	{
		tag:          "parsedir",
		translation:  "mount directory '{0}' is invalid. Check if source directory path exists.",
		validationFn: ParseMountDir,
	},
	{
		tag:         "required_without",
		translation: "image is required, unless the task has a `follow` field",
	},
}

// Validate validates config and returns errors.
func (configs *Configs) Validate() []error {
	err := initValidator(customValidations)
	if err != nil {
		return []error{err}
	}
	valErrs := govalidator.Struct(configs)
	errs := formatErrors(valErrs, "")
	ctx := context.WithValue(context.Background(), configsKey, configs)

	// Each step is validated separately so that task name can be added in error messages
	for taskName, task := range configs.Tasks {
		for _, steps := range task.Steps {
			taskValErrs := govalidator.VarCtx(ctx, steps, "dive")
			errs = append(errs, formatErrors(taskValErrs, taskName)...)
		}
	}
	return errs
}

func formatErrors(valErrs error, taskName string) []error {
	var errs []error
	if valErrs != nil {
		if _, ok := valErrs.(*validator.InvalidValidationError); ok {
			errs = append(errs, valErrs)
		} else {
			for _, e := range valErrs.(validator.ValidationErrors) {
				if taskName == "" {
					errs = append(errs, fmt.Errorf(e.Translate(trans)))
				} else {
					errs = append(errs, fmt.Errorf("task '%s': %s", taskName, e.Translate(trans)))
				}
			}
		}
	}
	return errs
}

func initValidator(customValidations []customValidation) error {
	govalidator = validator.New()
	govalidator.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("yaml"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})

	// Register default translators
	translator := en.New()
	uni = ut.New(translator, translator)
	var translatorFound bool
	trans, translatorFound = uni.GetTranslator("en")
	if !translatorFound {
		return fmt.Errorf("failed to initialize validator with translator")
	}
	en_translations.RegisterDefaultTranslations(govalidator, trans)

	// Register Custom validators and translations
	for _, t := range customValidations {
		if t.validationFn != nil {
			err := govalidator.RegisterValidationCtx(t.tag, t.validationFn)
			if err != nil {
				return fmt.Errorf("failed to register validation: %s", err.Error())
			}
		}
		err := govalidator.RegisterTranslation(t.tag, trans, registrationFunc(t.tag, t.translation), translateFunc)
		if err != nil {
			return fmt.Errorf("failed to register translations: %s", err.Error())
		}
	}
	return nil
}

// ValidateMountDir verifies that mount values are in proper format
//		<source>:<destination>:<mode>
// Format should match, <mode> is optional which is `readOnly` by default and `src` directory exists in host machine
func ValidateMountDir(ctx context.Context, fl validator.FieldLevel) bool {
	value := fl.Field().String()
	f := func(c rune) bool { return c == ':' }
	mountValues := strings.FieldsFunc(value, f)
	if len(mountValues) != 3 {
		mountValues = append(mountValues, defaultPermissionMode)
	}
	if len(mountValues) != 3 {
		return false
	}
	validPerm := false
	for _, perm := range validDirPermissionModes {
		if mountValues[len(mountValues)-1] == perm {
			validPerm = true
		}
	}
	return validPerm
}

// ValidateFollowTaskPresent verifies that referenceed task exists
func ValidateFollowTaskPresent(ctx context.Context, fl validator.FieldLevel) bool {
	followTask := strings.TrimSpace(fl.Field().String())
	configs := ctx.Value(configsKey).(*Configs)
	for taskName := range configs.Tasks {
		if taskName == followTask {
			return true
		}
	}
	return false
}

// ParseMountDir verifies that source directory exists and parses the environment variables used in the config
func ParseMountDir(ctx context.Context, fl validator.FieldLevel) bool {
	value := fl.Field().String()
	f := func(c rune) bool { return c == ':' }
	mountValues := strings.FieldsFunc(value, f)
	if len(mountValues) == 0 {
		return false
	}
	parsedDir, err := lookupDirectory(mountValues[0])
	if err != nil {
		return false
	}
	return util.DirExists(parsedDir)
}

// GetConfigs reads and parses tasks from the dunner task file.
// The task file is unmarshalled to an object of struct `Config`
// The default filename that is being read by Dunner during the time of execution is `dunner.yaml`,
// but it can be changed using `--task-file` flag in the CLI.
func GetConfigs(filename string) (*Configs, error) {
	taskFile, err := getDunnerTaskFile(filename)
	if err != nil {
		return nil, err
	}

	fileContents, err := ioutil.ReadFile(taskFile)
	if err != nil {
		return nil, err
	}

	var configs Configs
	if err := yaml.Unmarshal(fileContents, &configs); err != nil {
		return nil, err
	}

	loadDotEnv()
	if err := ParseEnvs(&configs); err != nil {
		return nil, err
	}

	return &configs, nil
}

// getDunnerTaskFile returns the dunner task file path.
// If `filename` is not default task file, it returns as-is.
// It returns task file in current directory if exists
// this routine keeps going upwards searching for task file
func getDunnerTaskFile(filename string) (string, error) {
	if internal.DefaultDunnerTaskFileName != filename {
		return filename, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	failErr := fmt.Errorf("failed to find Dunner task file")

	for {
		taskFile := filepath.Join(dir, internal.DefaultDunnerTaskFileName)
		if util.FileExists(taskFile) {
			return taskFile, nil
		}
		if dir == filepath.Clean(fmt.Sprintf("%c", os.PathSeparator)) || dir == "" {
			return "", failErr
		}
		oldDir := dir
		dir = filepath.Clean(fmt.Sprintf("%s%c..", dir, os.PathSeparator))
		if dir == oldDir {
			return "", failErr
		}
	}
}

func loadDotEnv() {
	file := viper.GetString("DotenvFile")
	var err error
	dotEnv, err = godotenv.Read(file)
	if err != nil {
		log.Infof("No environment loaded from %s file: Not found", file)
	}
}

// ParseEnvs parses the `.env` file as well as the host environment variables.
// If the same variable is defined in both the `.env` file and in the host environment,
// priority is given to the .env file.
//
// Note: You can change the filename of environment file (default: `.env`) using `--env-file/-e` flag in the CLI.
func ParseEnvs(configs *Configs) error {

	// Parse envs that are global to all
	for i, envVar := range (*configs).Envs {
		newEnv, err := obtainEnv(envVar)
		if err != nil {
			return err
		}
		(*configs).Envs[i] = newEnv
	}
	for k, tasks := range (*configs).Tasks {

		// Parse envs that are global to all steps of 'k' task
		for i, envVar := range tasks.Envs {
			newEnv, err := obtainEnv(envVar)
			if err != nil {
				return err
			}
			(*configs).Tasks[k].Envs[i] = newEnv
		}

		for j, step := range tasks.Steps {

			// Parse envs that are defined for an individual step
			for i, envVar := range step.Envs {
				newEnv, err := obtainEnv(envVar)
				if err != nil {
					return err
				}
				(*configs).Tasks[k].Steps[j].Envs[i] = newEnv
			}
		}
	}

	return nil
}

func obtainEnv(envVar string) (string, error) {
	var str = strings.Split(envVar, "=")
	if len(str) != 2 {
		return "", fmt.Errorf(
			`config: invalid format of environment variable: %v`,
			envVar,
		)
	}
	var pattern = "^`\\$.+`$"
	check, err := regexp.MatchString(pattern, str[1])
	if err != nil {
		log.Fatal(err)
	}
	if check {
		var key = strings.Replace(
			strings.Replace(
				str[1],
				"`",
				"",
				-1,
			),
			"$",
			"",
			1,
		)
		var val string
		// Value of variable defined in environment file (default '.env') overrides
		// the value defined in host's environment variables.
		if v, isSet := os.LookupEnv(key); isSet {
			val = v
		}
		if v, isSet := dotEnv[key]; isSet {
			val = v
		}
		if val == "" {
			return "", fmt.Errorf(
				`config: could not find environment variable '%v' in %s file or among host environment variables`,
				key,
				viper.GetString("DotenvFile"),
			)
		}
		var newEnv = str[0] + "=" + val
		return newEnv, nil
	}
	return envVar, nil
}

// ParseStepEnv parses Dir, Mounts, User fields of Step by replacing environment variables with their values
func (step *Step) ParseStepEnv() error {
	parsedDir, err := lookupDirectory(step.Dir)
	if err != nil {
		return err
	}
	step.Dir = parsedDir

	for index, m := range step.Mounts {
		parsedMount, err := lookupDirectory(m)
		if err != nil {
			return err
		}
		step.Mounts[index] = parsedMount
	}

	parsedUser, err := lookupDirectory(step.User)
	if err != nil {
		return err
	}
	step.User = parsedUser
	return nil
}

// DecodeMount parses mount format for directories to be mounted as bind volumes.
// The format to configure a mount is
// 		<source>:<destination>:<mode>
// By _mode_, the file permission level is defined in two ways, viz., _read-only_ mode(`r`) and _read-write_ mode(`wr` or `w`)
func DecodeMount(mounts []string, step *docker.Step) error {
	for _, m := range mounts {
		arr := strings.Split(
			strings.Trim(strings.Trim(m, `'`), `"`),
			":",
		)
		var readOnly = true
		if len(arr) == 3 {
			if arr[2] == "wr" || arr[2] == "w" {
				readOnly = false
			}
		}
		src, err := filepath.Abs(joinPathRelToHome(arr[0]))
		if err != nil {
			return err
		}

		(*step).ExtMounts = append((*step).ExtMounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   src,
			Target:   arr[1],
			ReadOnly: readOnly,
		})
	}
	return nil
}

// Replaces dir having any environment variables in form `$ENV_NAME` and returns a parsed string
func lookupDirectory(dir string) (string, error) {
	matches := hostDirRegex.FindAllStringSubmatch(dir, -1)

	parsedDir := dir
	for _, matchArr := range matches {
		envKey := matchArr[1]
		var val string
		if v, isSet := os.LookupEnv(envKey); isSet {
			val = v
		}
		if v, isSet := dotEnv[envKey]; isSet {
			val = v
		}
		if val == "" {
			return dir, fmt.Errorf("could not find environment variable '%v'", envKey)
		}
		parsedDir = strings.Replace(parsedDir, fmt.Sprintf("`$%s`", envKey), val, -1)
	}
	return parsedDir, nil
}

func joinPathRelToHome(p string) string {
	if p[0] == '~' {
		return path.Join(util.HomeDir, strings.Trim(p, "~"))
	}
	return p
}

func registrationFunc(tag string, translation string) validator.RegisterTranslationsFunc {
	return func(ut ut.Translator) (err error) {
		if err = ut.Add(tag, translation, true); err != nil {
			return
		}
		return
	}
}

func translateFunc(ut ut.Translator, fe validator.FieldError) string {
	t, err := ut.T(fe.Tag(), reflect.ValueOf(fe.Value()).String(), fe.Param())
	if err != nil {
		return fe.(error).Error()
	}
	return t
}
