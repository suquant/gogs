// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package cmd

import (
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Unknwon/com"
	"github.com/codegangsta/cli"

	"github.com/gogits/gogs/models"
	"github.com/gogits/gogs/modules/httplib"
	"github.com/gogits/gogs/modules/log"
	"github.com/gogits/gogs/modules/setting"
	"github.com/gogits/gogs/modules/uuid"
)

const (
	_ACCESS_DENIED_MESSAGE = "Repository does not exist or you do not have access"
)

var CmdServ = cli.Command{
	Name:        "serv",
	Usage:       "This command should only be called by SSH shell",
	Description: `Serv provide access auth for repositories`,
	Action:      runServ,
	Flags: []cli.Flag{
		stringFlag("config, c", "custom/conf/app.ini", "Custom configuration file path"),
	},
}

func setup(logPath string) {
	setting.NewContext()
	log.NewGitLogger(filepath.Join(setting.LogRootPath, logPath))

	if setting.DisableSSH {
		println("Gogs: SSH has been disabled")
		os.Exit(1)
	}

	models.LoadConfigs()

	if setting.UseSQLite3 || setting.UseTiDB {
		workDir, _ := setting.WorkDir()
		os.Chdir(workDir)
	}

	models.SetEngine()
}

func parseCmd(cmd string) (string, string) {
	ss := strings.SplitN(cmd, " ", 2)
	if len(ss) != 2 {
		return "", ""
	}
	return ss[0], strings.Replace(ss[1], "'/", "'", 1)
}

var (
	COMMANDS = map[string]models.AccessMode{
		"git-upload-pack":    models.ACCESS_MODE_READ,
		"git-upload-archive": models.ACCESS_MODE_READ,
		"git-receive-pack":   models.ACCESS_MODE_WRITE,
	}
)

func fail(userMessage, logMessage string, args ...interface{}) {
	fmt.Fprintln(os.Stderr, "Gogs:", userMessage)

	if len(logMessage) > 0 {
		log.GitLogger.Fatal(3, logMessage, args...)
		return
	}

	log.GitLogger.Close()
	os.Exit(1)
}

func handleUpdateTask(uuid string, user *models.User, repoUserName, repoName string) {
	task, err := models.GetUpdateTaskByUUID(uuid)
	if err != nil {
		if models.IsErrUpdateTaskNotExist(err) {
			log.GitLogger.Trace("No update task is presented: %s", uuid)
			return
		}
		log.GitLogger.Fatal(2, "GetUpdateTaskByUUID: %v", err)
	}

	if err = models.Update(task.RefName, task.OldCommitID, task.NewCommitID,
		user.Name, repoUserName, repoName, user.Id); err != nil {
		log.GitLogger.Error(2, "Update: %v", err)
	}

	if err = models.DeleteUpdateTaskByUUID(uuid); err != nil {
		log.GitLogger.Fatal(2, "DeleteUpdateTaskByUUID: %v", err)
	}

	// Ask for running deliver hook and test pull request tasks.
	reqURL := setting.AppUrl + repoUserName + "/" + repoName + "/tasks/trigger?branch=" +
		strings.TrimPrefix(task.RefName, "refs/heads/")
	log.GitLogger.Trace("Trigger task: %s", reqURL)

	resp, err := httplib.Head(reqURL).SetTLSClientConfig(&tls.Config{
		InsecureSkipVerify: true,
	}).Response()
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			log.GitLogger.Error(2, "Fail to trigger task: not 2xx response code")
		}
	} else {
		log.GitLogger.Error(2, "Fail to trigger task: %v", err)
	}
}

func runServ(c *cli.Context) {
	if c.IsSet("config") {
		setting.CustomConf = c.String("config")
	}
	setup("serv.log")

	if len(c.Args()) < 1 {
		fail("Not enough arguments", "Not enough arguments")
	}

	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if len(cmd) == 0 {
		println("Hi there, You've successfully authenticated, but Gogs does not provide shell access.")
		println("If this is unexpected, please log in with password and setup Gogs under another user.")
		return
	}

	verb, args := parseCmd(cmd)
	repoPath := strings.ToLower(strings.Trim(args, "'"))
	rr := strings.SplitN(repoPath, "/", 2)
	if len(rr) != 2 {
		fail("Invalid repository path", "Invalid repository path: %v", args)
	}
	repoUserName := strings.ToLower(rr[0])
	repoName := strings.ToLower(strings.TrimSuffix(rr[1], ".git"))

	repoUser, err := models.GetUserByName(repoUserName)
	if err != nil {
		if models.IsErrUserNotExist(err) {
			fail("Repository owner does not exist", "Unregistered owner: %s", repoUserName)
		}
		fail("Internal error", "Failed to get repository owner(%s): %v", repoUserName, err)
	}

	repo, err := models.GetRepositoryByName(repoUser.Id, repoName)
	if err != nil {
		if models.IsErrRepoNotExist(err) {
			fail(_ACCESS_DENIED_MESSAGE, "Repository does not exist: %s/%s", repoUser.Name, repoName)
		}
		fail("Internal error", "Failed to get repository: %v", err)
	}

	requestedMode, has := COMMANDS[verb]
	if !has {
		fail("Unknown git command", "Unknown git command %s", verb)
	}

	// Prohibit push to mirror repositories.
	if requestedMode > models.ACCESS_MODE_READ && repo.IsMirror {
		fail("mirror repository is read-only", "")
	}

	// Allow anonymous clone for public repositories.
	var (
		keyID int64
		user  *models.User
	)
	if requestedMode == models.ACCESS_MODE_WRITE || repo.IsPrivate {
		keys := strings.Split(c.Args()[0], "-")
		if len(keys) != 2 {
			fail("Key ID format error", "Invalid key argument: %s", c.Args()[0])
		}

		key, err := models.GetPublicKeyByID(com.StrTo(keys[1]).MustInt64())
		if err != nil {
			fail("Invalid key ID", "Invalid key ID[%s]: %v", c.Args()[0], err)
		}
		keyID = key.ID

		// Check deploy key or user key.
		if key.Type == models.KEY_TYPE_DEPLOY {
			if key.Mode < requestedMode {
				fail("Key permission denied", "Cannot push with deployment key: %d", key.ID)
			}
			// Check if this deploy key belongs to current repository.
			if !models.HasDeployKey(key.ID, repo.ID) {
				fail("Key access denied", "Key access denied: %d-%d", key.ID, repo.ID)
			}

			// Update deploy key activity.
			deployKey, err := models.GetDeployKeyByRepo(key.ID, repo.ID)
			if err != nil {
				fail("Internal error", "GetDeployKey: %v", err)
			}

			deployKey.Updated = time.Now()
			if err = models.UpdateDeployKey(deployKey); err != nil {
				fail("Internal error", "UpdateDeployKey: %v", err)
			}
		} else {
			user, err = models.GetUserByKeyID(key.ID)
			if err != nil {
				fail("internal error", "Failed to get user by key ID(%d): %v", keyID, err)
			}

			mode, err := models.AccessLevel(user, repo)
			if err != nil {
				fail("Internal error", "Fail to check access: %v", err)
			} else if mode < requestedMode {
				clientMessage := _ACCESS_DENIED_MESSAGE
				if mode >= models.ACCESS_MODE_READ {
					clientMessage = "You do not have sufficient authorization for this action"
				}
				fail(clientMessage,
					"User %s does not have level %v access to repository %s",
					user.Name, requestedMode, repoPath)
			}
		}
	}

	uuid := uuid.NewV4().String()
	os.Setenv("uuid", uuid)

	var gitcmd *exec.Cmd
	verbs := strings.Split(verb, " ")
	if len(verbs) == 2 {
		gitcmd = exec.Command(verbs[0], verbs[1], repoPath)
	} else {
		gitcmd = exec.Command(verb, repoPath)
	}
	gitcmd.Dir = setting.RepoRootPath
	gitcmd.Stdout = os.Stdout
	gitcmd.Stdin = os.Stdin
	gitcmd.Stderr = os.Stderr
	if err = gitcmd.Run(); err != nil {
		fail("Internal error", "Failed to execute git command: %v", err)
	}

	if requestedMode == models.ACCESS_MODE_WRITE {
		handleUpdateTask(uuid, user, repoUserName, repoName)
	}

	// Update user key activity.
	if keyID > 0 {
		key, err := models.GetPublicKeyByID(keyID)
		if err != nil {
			fail("Internal error", "GetPublicKeyById: %v", err)
		}

		key.Updated = time.Now()
		if err = models.UpdatePublicKey(key); err != nil {
			fail("Internal error", "UpdatePublicKey: %v", err)
		}
	}
}
