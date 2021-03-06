package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const baseSpace = "/root/src"

// Builder is
type Builder struct {
	// 用户提供参数, 通过环境变量传入
	GitCloneURL    string
	GitRef         string
	GitType        string
	Image          string
	ImageTagFormat string
	ImageTag       string
	ExtraImageTag  string
	BuildWorkdir   string
	DockerFilePath string
	BuildArgs      string

	HubUser  string
	HubToken string

	hub           string
	gitCommit     string
	gitTag        string
	gitCommitTime string
	projectName   string
}

// NewBuilder is
func NewBuilder(envs map[string]string) (*Builder, error) {
	b := &Builder{}

	if envs["_WORKFLOW_FLOW_URL"] == "" {
		return nil, fmt.Errorf("envionment variable _WORKFLOW_FLOW_URL is required")
	}
	paths := strings.Split(envs["_WORKFLOW_FLOW_URL"], "/")
	b.Image = strings.Join(paths[:len(paths) - 1], "/")
	if b.Image == "" {
		return nil, fmt.Errorf("envionment variable _WORKFLOW_FLOW_URL is invalid")
	}

	if envs["GIT_CLONE_URL"] != "" {
		b.GitCloneURL = envs["GIT_CLONE_URL"]
		b.GitRef = envs["GIT_REF"]
		b.GitType = envs["GIT_TYPE"]
	} else if envs["_WORKFLOW_GIT_CLONE_URL"] != "" {
		b.GitCloneURL = envs["_WORKFLOW_GIT_CLONE_URL"]
		b.GitRef = envs["_WORKFLOW_GIT_REF"]
		b.GitType = envs["_WORKFLOW_GIT_TYPE"]
	} else {
		return nil, fmt.Errorf("envionment variable GIT_CLONE_URL is required")
	}

	if b.GitRef == "" {
		b.GitRef = "master"
		b.GitType = "branch"
	}

	b.HubUser = envs["HUB_USER"]
	b.HubToken = envs["HUB_TOKEN"]

	if b.HubUser == "" && b.HubToken == "" {
		b.HubUser = envs["_WORKFLOW_HUB_USER"]
		b.HubToken = envs["_WORKFLOW_HUB_TOKEN"]
	}

	if b.HubUser == "" || b.HubToken == "" {
		return nil, fmt.Errorf("envionment variable HUB_USER, HUB_TOKEN are required")
	}

	if strings.Index(b.Image, ".") > -1 {
		b.hub = b.Image
	} else {
		b.hub = "index.docker.io" // default server
	}

	if envs["IMAGE_TAG"] != "" { // 高优先级
		b.ImageTag = envs["IMAGE_TAG"]
	} else {
		if envs["IMAGE_TAG_FORMAT"] == "" {
			b.ImageTag = "latest"
		} else {
			b.ImageTagFormat = envs["IMAGE_TAG_FORMAT"]
			// need GenImageTag
		}
	}

	s := strings.TrimSuffix(strings.TrimSuffix(b.GitCloneURL, "/"), ".git")
	b.projectName = s[strings.LastIndex(s, "/")+1:]

	b.ExtraImageTag = envs["EXTRA_IMAGE_TAG"]
	b.BuildWorkdir = envs["BUILD_WORKDIR"]
	b.DockerFilePath = envs["DOCKERFILE_PATH"]
	b.BuildArgs = envs["BUILD_ARGS"]

	return b, nil
}

func (b *Builder) run() error {
	if err := os.Chdir(baseSpace); err != nil {
		return fmt.Errorf("Chdir to baseSpace(%s) failed:%v", baseSpace, err)
	}

	if err := b.gitPull(); err != nil {
		return err
	}

	if err := b.gitReset(); err != nil {
		return err
	}

	if b.ImageTag == "" && b.ImageTagFormat != "" {
		if err := b.GenImageTag(); err != nil {
			return err
		}
	}

	if err := b.loginRegistry(); err != nil {
		return err
	}

	imageURL := fmt.Sprintf("%s:%s", b.Image, b.ImageTag)
	if err := b.build(imageURL); err != nil {
		return err
	}
	if err := b.push(imageURL); err != nil {
		return err
	}

	if b.ExtraImageTag != "" {
		newImageURL := fmt.Sprintf("%s:%s", b.Image, b.ExtraImageTag)
		if err := b.newTag(imageURL, newImageURL); err != nil {
			return err
		}
		if err := b.push(newImageURL); err != nil {
			return err
		}
		if err := b.cleanImage(newImageURL); err != nil {
			return err
		}
	}

	if err := b.pluckImageID(imageURL); err != nil {
		return err
	}

	if err := b.pluckImageDigest(imageURL); err != nil {
		return err
	}

	fmt.Printf("[JOB_OUT] IMAGE = %s\n", b.Image)
	fmt.Printf("[JOB_OUT] IMAGE_TAG = %s\n", b.ImageTag)

	if err := b.cleanImage(imageURL); err != nil {
		return err
	}
	return nil
}

func (b *Builder) gitPull() error {
	var command = []string{"git", "clone", "--recurse-submodules", b.GitCloneURL, b.projectName}
	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("Clone project failed:", err)
		return err
	}
	fmt.Println("Clone project", b.GitCloneURL, "succeded.")
	return nil
}

func (b *Builder) gitReset() error {
	cwd, _ := os.Getwd()
	var command = []string{"git", "checkout", b.GitRef}
	if _, err := (CMD{command, filepath.Join(cwd, b.projectName)}).Run(); err != nil {
		fmt.Println("Switch to git ref ", b.GitRef, "failed:", err)
		return err
	}
	fmt.Println("Switch to", b.GitRef, "succeded.")
	return nil
}

func (b *Builder) GenImageTag() error {
	var commitID, branchOrTag string
	cwd, _ := os.Getwd()

	// Get commit ID
	if b.GitType != "commit" {
		command := []string{"git", "show", "-s", "--format=%H", b.GitRef}
		output, err := (CMD{command, filepath.Join(cwd, b.projectName)}).Run()
		if err != nil {
			fmt.Println("get git commit id failed:", err)
			return err
		}
		output = strings.TrimSpace(output)
		if len(output) > 0 {
			commitID = output
		} else {
			return errors.New("can not get git commit id")
		}
	}

	if b.GitType == "tag" || b.GitType == "branch" {
		branchOrTag = b.GitRef
	}

	tag, err := GenImageTag(b.ImageTagFormat, branchOrTag, commitID)
	if err != nil {
		fmt.Println("GenImageTag failed:", err)
		return err
	}

	b.ImageTag = tag

	fmt.Println("GenImageTag", b.ImageTag, "succeded.")
	return nil
}

func (b *Builder) loginRegistry() error {
	var command = []string{"docker", "login", b.hub, "-u", b.HubUser, "-p", b.HubToken}
	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("docker login failed:", err)
		return err
	}
	fmt.Println("docker login succ.")
	return nil
}

func (b *Builder) build(imageURL string) error {
	var contextDir = filepath.Join(baseSpace, b.projectName, b.BuildWorkdir)
	var dockerfilePath string
	if b.DockerFilePath != "" {
		dockerfilePath = filepath.Join(baseSpace, b.projectName, b.DockerFilePath)
	}

	var command = []string{"docker", "build"}
	// var command = []string{"docker", "build", "--pull"}

	if dockerfilePath != "" {
		command = append(command, "--file", dockerfilePath)
	}
	command = append(command, "--tag", imageURL)

	if b.BuildArgs != "" {
		args := map[string]string{}
		err := json.Unmarshal([]byte(b.BuildArgs), &args)
		if err != nil {
			fmt.Println("Unmarshal BUILD_ARG error: ", err)
		} else {
			for k, v := range args {
				command = append(command, "--build-arg", fmt.Sprintf("%s=%s", k, v))
			}
		}
	}

	command = append(command, contextDir)

	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("Run docker build failed:", err)
		return err
	}
	fmt.Println("Run docker build succeded.")
	return nil
}

func (b *Builder) push(imageURL string) error {
	var command = []string{"docker", "push", imageURL}
	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("Run docker push failed:", err)
		return err
	}
	fmt.Println("Run docker push succeded.")
	return nil
}

func (b *Builder) newTag(old, new string) error {
	var command = []string{"docker", "tag", old, new}
	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("Run docker tag failed:", err)
		return err
	}
	fmt.Println("Run docker tag succeded.")
	return nil
}

func (b *Builder) pluckImageID(imageURL string) error {
	// docker inspect hub.cloud.tencent.com/tencenthub/docker_builder:latest --format '{{.Id}}'
	var command = []string{"docker", "inspect", imageURL, "--format", "{{.Id}}"}
	// docker images ccr.ccs.tencentyun.com/tencenthub/workflow:latest --format "{{.ID}}"
	// var command = []string{"docker", "images", b.Image, "--format", "{{.ID}}"}
	output, err := (CMD{Command: command}).Run()

	if err != nil {
		fmt.Println("pluck image id failed:", err)
		return err
	}
	if len(output) > 0 {
		fmt.Println("pluck image id succeded.")
		fmt.Printf("[JOB_OUT] IMAGE_ID = %s", output)
	} else {
		return errors.New("Can not get image id")
	}

	return nil
}

func (b *Builder) pluckImageDigest(imageURL string) error {
	// docker inspect hub.cloud.tencent.com/tencenthub/docker_builder:latest --format '{{index .RepoDigests 0}}'
	var command = []string{"docker", "inspect", imageURL, "--format", "{{index .RepoDigests 0}}"}
	output, err := (CMD{Command: command}).Run()

	if err != nil {
		fmt.Println("pluck image digest failed:", err)
		return err
	}
	cut := b.Image + "@"
	output = strings.TrimPrefix(output, cut)
	if len(output) > 0 {
		fmt.Println("pluck image digest succeded.")
		fmt.Printf("[JOB_OUT] IMAGE_DIGEST = %s\n", output)
	} else {
		return errors.New("Can not get image digest")
	}

	return nil
}

func (b *Builder) cleanImage(imageURL string) error {
	var command = []string{"docker", "rmi", imageURL}
	if _, err := (CMD{Command: command}).Run(); err != nil {
		fmt.Println("Run docker rmi", imageURL, "failed:", err)
		return err
	}
	fmt.Println("clean local image completely.")
	return nil
}

func ensureDirExists(dir string) (err error) {
	f, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, os.FileMode(0755))
		}
		return err
	}

	if !f.IsDir() {
		return fmt.Errorf("%s is not dir", dir)
	}

	return nil
}

type CMD struct {
	Command []string // cmd with args
	WorkDir string
}

func (c CMD) Run() (string, error) {
	fmt.Println("Run CMD: ", strings.Join(c.Command, " "))

	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	if c.WorkDir != "" {
		cmd.Dir = c.WorkDir
	}

	data, err := cmd.CombinedOutput()
	result := string(data)
	if len(result) > 0 {
		fmt.Println(result)
	}

	return result, err
}
