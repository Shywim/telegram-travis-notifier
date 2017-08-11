package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/garyburd/redigo/redis"
	"gopkg.in/telegram-bot-api.v4"
)

type travisRepo struct {
	ID          int64
	Slug        string
	Description string
	LastBuildID int64 `json:"last_build_id"`
}

type travisInfos struct {
	*travisRepo
	LastBuildNumber     string    `json:"last_build_number"`
	LastBuildStatus     int       `json:"last_build_status"`
	LastBuildResult     int       `json:"last_build_result"`
	LastBuildDuration   int64     `json:"last_build_duration"`
	LastBuildStartedAt  time.Time `json:"last_build_started_at"`
	LastBuildFinishedAt time.Time `json:"last_build_finished_at"`
}

const (
	travisURL = "https://api.travis-ci.org/repositories/"

	dbKey              = "teletravis"
	dbKeyRepos         = "teletravis:data:repos"
	dbKeyRepo          = "teletravis:data:repo:%d"
	dbKeyRepoUsers     = "teletravis:data:repo:%d:users"
	dbKeyUserRepos     = "teletravis:data:user:%d:repos"
	dbKeyUserLastBuild = "teletravis:data:user:%d:repo:%d:lastbuild"

	msgStart = "Hey there!\n" +
		"To get started, use the `/add` command to send me a public github repository link or a " +
		"`username/repo` text, and I'll let you know of future Travis builds!\n\n" +
		"⚠️ _️Case is important!_"
	msgRepoAdded = "Repository *%s* successfully added!\n" +
		"I will now notify you of future build results. 🎉"
	msgRepoBuild = "[*%s*](%s)* #%s*\n\nLast build %s\nDate: %s\nRun time: %v\n\n%s"

	strBuildPassed = "_passed_ ✔"
	strBuildFailed = "_failed_ ❌"
)

var (
	bot         *tgbotapi.BotAPI
	ghLinkRegex *regexp.Regexp
	ghSlugRegex *regexp.Regexp
	redisPool   *redis.Pool
)

func utilInt64s(reply interface{}, err error) ([]int64, error) {
	var ints []int64
	values, err := redis.Values(reply, err)
	if err != nil {
		return ints, err
	}
	if err := redis.ScanSlice(values, &ints); err != nil {
		return ints, err
	}
	return ints, nil
}

func showStartMessage(c *tgbotapi.Chat) {
	msg := tgbotapi.NewMessage(c.ID, msgStart)
	msg.ParseMode = tgbotapi.ModeMarkdown
	bot.Send(msg)
}

func showHelp(m *tgbotapi.Message) {
	showStartMessage(m.Chat)
}

func getRepoInfos(url string) *travisInfos {
	resp, err := http.Get(url)
	if err != nil {
		log.WithError(err).Error("Failed to fetch repo data")
		return nil
	}

	repoInfos := &travisInfos{}

	body, err := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, repoInfos)
	if err != nil {
		log.WithError(err).Error("Failed to fetch repo data")
		return nil
	}

	return repoInfos
}

func getRepoInfosID(id int64) *travisInfos {
	return getRepoInfos(fmt.Sprintf("%s%d", travisURL, id))
}

func getRepoInfosName(repo string) *travisInfos {
	return getRepoInfos(fmt.Sprintf("%s%s", travisURL, repo))
}

func checkRepoExists(repo string) (bool, string) {
	resp, err := http.Get(fmt.Sprintf("%s%s", travisURL, repo))
	if err != nil {
		log.WithError(err).Error("Failed to fetch repo data")
		return false, "Unable to check repository"
	}

	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case http.StatusNotFound:
			return false, ""
		case http.StatusInternalServerError:
			return false, "It seems travis-ci.org are unavailable, check back later!"
		default:
			return false, "Unable to check repository"
		}
	}

	return true, ""
}

func getRepoSlug(s string) string {
	match := ghLinkRegex.FindString(s)
	if match == "" {
		match = ghSlugRegex.FindString(s)
	}
	if match == "" {
		return ""
	}

	repo := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match, "http://"), "https://"), ".git")
	repoSplit := strings.SplitN(repo, "/", 2)

	return repoSplit[1]
}

func checkForGithubLink(m *tgbotapi.Message, arg string) {
	repoSlug := getRepoSlug(arg)

	exists, err := checkRepoExists(repoSlug)
	if err != "" {
		msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("Oops! I encountered an error while verifying %s existence: %s", repoSlug, err))
		msg.ReplyToMessageID = m.MessageID
		bot.Send(msg)
		return
	}

	if !exists {
		msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("It appears that %s doesn't exists on travis-ci.org!", repoSlug))
		msg.ReplyToMessageID = m.MessageID
		bot.Send(msg)
		return
	}

	repoInfo := getRepoInfosName(repoSlug)
	serialized, _ := json.Marshal(repoInfo)

	conn := redisPool.Get()
	defer conn.Close()
	conn.Do("SADD", fmt.Sprintf(dbKeyUserRepos, m.Chat.ID), repoInfo.ID)
	conn.Do("SADD", fmt.Sprintf(dbKeyRepoUsers, repoInfo.ID), m.Chat.ID)
	conn.Do("SADD", dbKeyRepos, repoInfo.ID)
	conn.Do("SET", fmt.Sprintf(dbKeyRepo, repoInfo.ID), serialized)
	conn.Do("SET", fmt.Sprintf(dbKeyUserLastBuild, m.Chat.ID, repoInfo.ID), repoInfo.LastBuildID)

	msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf(msgRepoAdded, repoInfo.Slug))
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.ReplyToMessageID = m.MessageID
	bot.Send(msg)
}

func sendErrUnableToGetBuild(m *tgbotapi.Message, repoSlug string) {
	msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("I could not get build info for *%s* 🙁", repoSlug))
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.ReplyToMessageID = m.MessageID
	bot.Send(msg)
}

func sendBuildInfos(chatID int64, repoBuild *travisInfos, m *tgbotapi.Message) {
	var result string
	if repoBuild.LastBuildStatus == 0 {
		result = strBuildPassed
	} else {
		result = strBuildFailed
	}

	repoURL := fmt.Sprintf("%s%d", travisURL, repoBuild.ID)
	msg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf(msgRepoBuild, repoBuild.Slug, repoURL, repoBuild.LastBuildNumber,
			result, repoBuild.LastBuildStartedAt.Format("2006-01-02 15:04"),
			repoBuild.LastBuildFinishedAt.Sub(repoBuild.LastBuildStartedAt), repoURL))
	msg.ParseMode = tgbotapi.ModeMarkdown
	if m != nil {
		msg.ReplyToMessageID = m.MessageID
	}
	bot.Send(msg)
}

func checkBuild(m *tgbotapi.Message, arg string) {
	repoSlug := arg

	if repoSlug == "" {
		showHelp(m)
		return
	}

	repoBuild := getRepoInfosName(repoSlug)
	if repoBuild == nil {
		sendErrUnableToGetBuild(m, repoSlug)
		return
	}

	sendBuildInfos(m.Chat.ID, repoBuild, m)
}

func handleMessage(m *tgbotapi.Message) {
	if !m.IsCommand() {
		showHelp(m)
		return
	}

	cmd := m.Command()
	switch cmd {
	case "start":
	case "help":
		showStartMessage(m.Chat)
		break

	case "add":
		checkForGithubLink(m, m.CommandArguments())
		break

	case "get":
		checkBuild(m, m.CommandArguments())
		break

	case "remove":
	case "list":
	case "settings":
	case "about":
		msg := tgbotapi.NewMessage(m.Chat.ID, "not implemented")
		msg.ReplyToMessageID = m.MessageID
		bot.Send(msg)
	}
}

func getRepos(c *redis.Conn) []int64 {
	reply, err := (*c).Do("SMEMBERS", dbKeyRepos)
	if reply == nil && err == nil {
		log.Warn("No repository in redis database")
		return nil
	}

	repos, err := utilInt64s(reply, err)
	if err != nil {
		log.WithError(err).Fatal("Failed to retrieve redis keys")
	}

	return repos
}

func launchUpdateLoop() {
	t := time.NewTicker(5 * time.Minute)
	for {
		conn := redisPool.Get()

		repos := getRepos(&conn)
		if repos == nil {
			<-t.C
			continue
		}

		for _, repo := range repos {
			repoInfo := &travisRepo{}

			reply, err := conn.Do("GET", fmt.Sprintf(dbKeyRepo, repo))
			data, err := redis.Bytes(reply, err)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"repo":  repo,
				}).Error("Error getting repo infos from redis")
				continue
			}

			json.Unmarshal(data, repoInfo)

			repoBuild := getRepoInfosID(repoInfo.ID)

			reply, err = conn.Do("SMEMBERS", fmt.Sprintf(dbKeyRepoUsers, repo))
			users, err := utilInt64s(reply, err)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"repo":  repo,
				}).Error("Error getting user infos from redis")
				continue
			}

			for _, user := range users {
				reply, err = conn.Do("GET", fmt.Sprintf(dbKeyUserLastBuild, user, repoInfo.ID))
				lastBuildID, err := redis.Int64(reply, err)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err,
						"repo":  repo,
					}).Error("Error getting build infos from redis")
					continue
				}

				if lastBuildID != repoBuild.LastBuildID {
					serialized, _ := json.Marshal(repoBuild)
					conn.Do("SET", fmt.Sprintf(dbKeyRepo, repoInfo.ID), serialized)
					conn.Do("SET", fmt.Sprintf(dbKeyUserLastBuild, user, repoBuild.ID), repoBuild.LastBuildID)

					sendBuildInfos(user, repoBuild, nil)
				}
			}
		}

		conn.Close()

		<-t.C
	}
}

func init() {
	ghLinkRegex = regexp.MustCompile(".*github.com/[A-Za-z0-9_.-]*/[A-Za-z0-9_.-]*")
	ghSlugRegex = regexp.MustCompile("[A-Za-z0-9_.-]*/[A-Za-z0-9_.-]*")
}

func main() {
	token := flag.String("token", "", "Bot authentication token")
	redisAddr := flag.String("redis", "localhost:6379", "Redis address")
	debug := flag.Bool("debug", false, "Enable debug")

	flag.Parse()

	redisPool = &redis.Pool{
		MaxIdle:     5,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			return redis.Dial("tcp", *redisAddr)
		},
	}

	var err error
	bot, err = tgbotapi.NewBotAPI(*token)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"token": *token,
		}).Fatal("Could not connect to telegram")
	}

	bot.Debug = *debug

	log.Info("Telegram Travis Notifier is running with bot account ", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)

	go launchUpdateLoop()

	for update := range updates {
		if update.Message == nil {
			continue
		}

		go handleMessage(update.Message)
	}
}
