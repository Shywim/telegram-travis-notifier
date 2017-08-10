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
)

var (
	bot         *tgbotapi.BotAPI
	ghLinkRegex *regexp.Regexp
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
	msg := tgbotapi.NewMessage(c.ID, "Hey there!\nTo get started, just send me a public github repository link, and I'll let you know of future Travis builds!")
	bot.Send(msg)
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

func checkForGithubLink(m *tgbotapi.Message) {
	matches := ghLinkRegex.FindAllString(m.Text, -1)
	if matches == nil {
		return
	}

	for _, g := range matches {
		repo := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(g, "http://"), "https://"), ".git")
		repoSplit := strings.SplitN(repo, "/", 2)

		exists, err := checkRepoExists(repoSplit[1])
		if err != "" {
			msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("Oops! I encountered an error while verifying %s existence: %s", repoSplit[1], err))
			bot.Send(msg)
			continue
		}

		if !exists {
			msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("It appears that %s doesn't exists on travis-ci.org!", repoSplit[1]))
			bot.Send(msg)
			continue
		}

		repoInfo := getRepoInfosName(repoSplit[1])
		serialized, _ := json.Marshal(repoInfo)

		conn := redisPool.Get()
		defer conn.Close()
		conn.Do("SADD", fmt.Sprintf("teletravis:data:user:%d:repos", m.Chat.ID), repoInfo.ID)
		conn.Do("SADD", fmt.Sprintf("teletravis:data:repo:%d:users", repoInfo.ID), m.Chat.ID)
		conn.Do("SADD", "teletravis:data:repos", repoInfo.ID)
		conn.Do("SET", fmt.Sprintf("teletravis:data:repo:%d", repoInfo.ID), serialized)
		conn.Do("SET", fmt.Sprintf("teletravis:data:user:%d:repo:%d:lastbuild", m.Chat.ID, repoInfo.ID), repoInfo.LastBuildID)

		msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf("Repository **%s** successfully added!\nI will now notify you of future build results. 🎉", repoInfo.Slug))
		msg.ParseMode = tgbotapi.ModeMarkdown
		bot.Send(msg)
	}
}

func handleMessage(m *tgbotapi.Message) {
	switch m.Text {
	case "/start":
		showStartMessage(m.Chat)
		break

	default:
		checkForGithubLink(m)
		break
	}
}

func launchUpdateLoop() {
	t := time.NewTicker(5 * time.Minute)
	for {
		conn := redisPool.Get()

		reply, err := conn.Do("SMEMBERS", "teletravis:data:repos")
		if reply == nil && err == nil {
			log.Warn("No repository in redis database")
			<-t.C
			continue
		}

		repos, err := utilInt64s(reply, err)
		if err != nil {
			log.WithError(err).Fatal("Failed to retrieve redis keys")
			return
		}

		for _, repo := range repos {
			repoInfo := &travisRepo{}

			reply, err := conn.Do("GET", fmt.Sprintf("teletravis:data:repo:%d", repo))
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

			reply, err = conn.Do("SMEMBERS", fmt.Sprintf("teletravis:data:repo:%d:users", repo))
			users, err := utilInt64s(reply, err)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err,
					"repo":  repo,
				}).Error("Error getting user infos from redis")
				continue
			}

			for _, user := range users {
				reply, err = conn.Do("GET", fmt.Sprintf("teletravis:data:user:%d:repo:%d:lastbuild", user, repoInfo.ID))
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
					conn.Do("SET", fmt.Sprintf("teletravis:data:repo:%d", repoInfo.ID), serialized)
					conn.Do("SET", fmt.Sprintf("teletravis:data:user:%d:repo:%d:lastbuild", user, repoBuild.ID), repoBuild.LastBuildID)

					var result string
					if repoBuild.LastBuildStatus == 0 {
						result = "*passed* ✔"
					} else {
						result = "*failed* ❌"
					}

					repoURL := fmt.Sprintf("%s%d", travisURL, repoBuild.ID)
					msg := tgbotapi.NewMessage(user,
						fmt.Sprintf("**[%s](%s) #%s**\n\nLast build %s\nDate: %s\nRun time: %v\n\n%s",
							repoBuild.Slug, repoURL, repoBuild.LastBuildNumber, result, repoBuild.LastBuildStartedAt.Format("2006-01-02 15:04"),
							repoBuild.LastBuildFinishedAt.Sub(repoBuild.LastBuildStartedAt), repoURL))
					msg.ParseMode = tgbotapi.ModeMarkdown
					bot.Send(msg)
				}
			}
		}

		conn.Close()

		<-t.C
	}
}

func init() {
	ghLinkRegex = regexp.MustCompile(".*github.com/[A-Za-z0-9_.-]*/[A-Za-z0-9_.-]*")
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
