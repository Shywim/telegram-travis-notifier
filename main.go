package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/garyburd/redigo/redis"
	"gopkg.in/telegram-bot-api.v4"
)

type travisRepo struct {
	ID                  int64
	Slug                string
	Description         string
	LastBuildID         int64     `json:"last_build_id"`
	LastBuildNumber     string    `json:"last_build_number"`
	LastBuildStatus     int       `json:"last_build_status"`
	LastBuildResult     int       `json:"last_build_result"`
	LastBuildDuration   int64     `json:"last_build_duration"`
	LastBuildStartedAt  time.Time `json:"last_build_started_at"`
	LastBuildFinishedAt time.Time `json:"last_build_finished_at"`
}

func newTravisRepo() *travisRepo {
	t := new(travisRepo)
	t.LastBuildStatus = -1
	t.LastBuildResult = -1
	return t
}

const (
	travisURL    = "https://travis-ci.org/"
	travisURLApi = "https://api.travis-ci.org/repositories/"

	dbKey              = "teletravis"
	dbKeyRepos         = "teletravis:data:repos"
	dbKeyRepo          = "teletravis:data:repo:%d"
	dbKeyRepoUsers     = "teletravis:data:repo:%d:users"
	dbKeyUserRepos     = "teletravis:data:user:%d:repos"
	dbKeyUserLastBuild = "teletravis:data:user:%d:repo:%d:lastbuild"

	msgStart = "Hey there!\n" +
		"To get started, use the /add command to send me a public github repository link or a " +
		"`username/repo` text, and I'll let you know of future Travis builds!\n\n" +
		"⚠️ _️Case is important!_"
	msgRepoAdded = "Repository *%s* successfully added!\n" +
		"I will now notify you of future build results. 🎉"
	msgRepoBuild     = "*%s #%s*\n\nLast build %s\nDate: %s\nRun time: %v\n\n%s"
	msgRepoList      = "Here are the repositories I am watching for you:\n"
	msgDeleteSuccess = "*%s* has been deleted from the watchlist successfully!"
	msgHelp          = "I will notify you of new builds in your travis projects.\n\n" +
		"*Usage*\n" +
		"- /add `username/repo` | add a repository from github\n" +
		"- /list | list the repositories I am watching for you\n" +
		"- /remove `username/repo` | stop watching a repository\n" +
		"- /get `username/repo` | fetch a repository's build status\n" +
		"\n" +
		"- /help | show this message\n" +
		"- /about | show informations about my owner\n" +
		"\n" +
		"Note that the watch list is bound to the _conversation_, this enable you to added this bot " +
		"to another conversation and have a specific repository list for this conversation!"
	msgAbout = "My owner is Matthieu Harlé, aka Shywim.\n\n" +
		"If you have any issue with this bot, please report on it on the issue tracker: " +
		"https://github.com/Shywim/telegram-travis-notifier\n\n" +
		"Twitter - https://twitter.com/Shywim\n" +
		"Github - https://github.com/Shywim\n" +
		"Blog - https://blog.shywim.fr"

	strBuildPassed  = "*passed* ✔"
	strBuildFailed  = "*failed* ❌"
	strRepoListItem = "\n- [%s](%s)"
)

var (
	bot           *tgbotapi.BotAPI
	ghLinkRegex   *regexp.Regexp
	ghSlugRegex   *regexp.Regexp
	dataNextRegex *regexp.Regexp
	dataPrevRegex *regexp.Regexp
	redisPool     *redis.Pool
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

func sendStartMessage(c *tgbotapi.Chat) {
	msg := tgbotapi.NewMessage(c.ID, msgStart)
	msg.ParseMode = tgbotapi.ModeMarkdown
	bot.Send(msg)
}

func sendHelp(m *tgbotapi.Message) {
	msg := tgbotapi.NewMessage(m.Chat.ID, msgHelp)
	msg.ParseMode = tgbotapi.ModeMarkdown
	bot.Send(msg)
}

func sendAbout(m *tgbotapi.Message) {
	msg := tgbotapi.NewMessage(m.Chat.ID, msgAbout)
	msg.ParseMode = tgbotapi.ModeMarkdown
	bot.Send(msg)
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
	if strings.Count(repo, "/") > 1 {
		repoSplit := strings.SplitN(repo, "/", 2)
		return repoSplit[1]
	}
	return repo
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
	if repoInfo == nil {
		sendErrUnableToGetBuild(m, repoSlug)
		return
	}
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

func sendErrUnableToListRepos(m *tgbotapi.Message) {
	msg := tgbotapi.NewMessage(m.Chat.ID, "I could not retrieve your repository list 🙁")
	msg.ReplyToMessageID = m.MessageID
	bot.Send(msg)
}

func sendBuildInfos(chatID int64, repoBuild *travisRepo, m *tgbotapi.Message) {
	var result string
	if repoBuild.LastBuildStatus == 0 {
		result = strBuildPassed
	} else {
		result = strBuildFailed
	}

	repoURL := fmt.Sprintf("%s%s", travisURL, repoBuild.Slug)
	msg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf(msgRepoBuild, repoBuild.Slug, repoBuild.LastBuildNumber,
			result, repoBuild.LastBuildStartedAt.Format("2006-01-02 15:04"),
			repoBuild.LastBuildFinishedAt.Sub(repoBuild.LastBuildStartedAt), repoURL))
	msg.ParseMode = tgbotapi.ModeMarkdown
	if m != nil {
		msg.ReplyToMessageID = m.MessageID
	}
	bot.Send(msg)
}

func sendEmptyList(m *tgbotapi.Message) {
	msg := tgbotapi.NewMessage(m.Chat.ID, "Your repository list is empty, start by adding one with /add!")
	msg.ReplyToMessageID = m.MessageID
	bot.Send(msg)
}

func sendSuccessDelete(m *tgbotapi.Message, s string) {
	msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf(msgDeleteSuccess, s))
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.ReplyToMessageID = m.MessageID
	bot.Send(msg)
}

func checkBuild(m *tgbotapi.Message, arg string) {
	repoSlug := arg

	if repoSlug == "" {
		sendHelp(m)
		return
	}

	repoBuild := getRepoInfosName(repoSlug)
	if repoBuild == nil {
		sendErrUnableToGetBuild(m, repoSlug)
		return
	}

	sendBuildInfos(m.Chat.ID, repoBuild, m)
}

//func proposeDeleteRepos(m *tgbotapi.Message) {
//	conn := redisPool.Get()
//	defer conn.Close()
//
//	repos := getUserReposFromDb(&conn, m.Chat.ID)
//
//	buttons := [][]tgbotapi.KeyboardButton{}
//	for i, repo := range repos {
//		button := tgbotapi.NewKeyboardButton(repo.Slug)
//		buttons[i%2] = append(buttons[i%2], button)
//
//		if i == 7 {
//			break
//		}
//	}
//
//	keyboard := tgbotapi.NewReplyKeyboard(buttons...)
//	keyboard.OneTimeKeyboard = true
//	keyboard.Selective = true
//	keyboard.ResizeKeyboard = true
//
//	msg := tgbotapi.NewMessage(m.Chat.ID, "Pick the repo you want to delete or write its name")
//	msg.ReplyMarkup = keyboard
//	msg.ReplyToMessageID = m.MessageID
//	bot.Send(msg)
//}

func removeRepo(m *tgbotapi.Message, args string) {
	//	if args == "" {
	//		proposeDeleteRepos(m)
	//		return
	//	}

	if !ghSlugRegex.MatchString(args) {
		sendHelp(m)
		return
	}

	repoInfos := getRepoInfosName(args)

	conn := redisPool.Get()
	defer conn.Close()
	deleteRepoFromDb(&conn, m.Chat.ID, repoInfos.ID)

	sendSuccessDelete(m, args)
}

func listRepos(m *tgbotapi.Message) {
	conn := redisPool.Get()
	defer conn.Close()

	repos, hasMore := getRepoListPage(0, m.Chat.ID)
	if repos == nil {
		sendErrUnableToListRepos(m)
		return
	} else if len(repos) == 0 {
		sendEmptyList(m)
		return
	}

	keyboard := repoListToKeyboardMarkup(repos, 0, hasMore)

	msg := tgbotapi.NewMessage(m.Chat.ID, fmt.Sprintf(msgRepoList))
	msg.ParseMode = tgbotapi.ModeMarkdown
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleMessage(m *tgbotapi.Message) {
	if !m.IsCommand() {
		sendHelp(m)
		return
	}

	cmd := m.Command()
	switch cmd {
	case "start":
		sendStartMessage(m.Chat)
		break

	case "help":
		sendHelp(m)
		break

	case "add":
		checkForGithubLink(m, m.CommandArguments())
		break

	case "get":
		checkBuild(m, m.CommandArguments())
		break

	case "remove":
		removeRepo(m, m.CommandArguments())
		break

	case "list":
		listRepos(m)
		break

	case "about":
		sendAbout(m)
		break

	case "settings":
		msg := tgbotapi.NewMessage(m.Chat.ID, "not implemented")
		msg.ReplyToMessageID = m.MessageID
		bot.Send(msg)
	}
}

func repoListToKeyboardMarkup(repos []*travisRepo, page int, hasMore bool) tgbotapi.InlineKeyboardMarkup {
	buttons := [][]tgbotapi.InlineKeyboardButton{}
	for i, repo := range repos {
		if i%2 == 0 {
			buttons = append(buttons, []tgbotapi.InlineKeyboardButton{})
		}
		buttons[i/2] = append(buttons[i/2], tgbotapi.NewInlineKeyboardButtonData(repo.Slug, repo.Slug))
	}
	if hasMore && page > 0 {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("◀ Previous", fmt.Sprintf("PREV%d", page-1)),
			tgbotapi.NewInlineKeyboardButtonData("Next ▶", fmt.Sprintf("NEXT%d", page+1))))
	} else if hasMore && page == 0 {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Next ▶", fmt.Sprintf("NEXT%d", page+1))))
	} else if !hasMore && page > 0 {
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("◀ Previous", fmt.Sprintf("PREV%d", page-1))))
	}
	return tgbotapi.NewInlineKeyboardMarkup(buttons...)
}

func handleCallbackQuery(u *tgbotapi.Update) {
	cq := u.CallbackQuery
	data := cq.Data

	if dataNextRegex.MatchString(data) || dataPrevRegex.MatchString(data) {
		page, _ := strconv.Atoi(strings.TrimLeft(strings.TrimLeft(data, "NEXT"), "PREV"))
		repos, hasMore := getRepoListPage(page, cq.Message.Chat.ID)

		keyboard := repoListToKeyboardMarkup(repos, page, hasMore)

		edit := tgbotapi.NewEditMessageReplyMarkup(cq.Message.Chat.ID, cq.Message.MessageID, keyboard)
		bot.Send(edit)
	} else if data == "BACK" {
		repos, hasMore := getRepoListPage(0, cq.Message.Chat.ID)

		keyboard := repoListToKeyboardMarkup(repos, 0, hasMore)

		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, msgRepoList)
		edit.ParseMode = tgbotapi.ModeMarkdown
		edit.ReplyMarkup = &keyboard
		bot.Send(edit)
	} else {
		if strings.HasPrefix(data, "CHECK:") {
			data = strings.TrimPrefix(data, "CHECK:")
		}

		repoBuild := getRepoInfosName(data)
		topRow := tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Check", fmt.Sprintf("CHECK:%s", data)),
			tgbotapi.NewInlineKeyboardButtonData("Remove", fmt.Sprintf("REMOVE:%s", data)))
		bottomRow := tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("⏪ Back to list", "BACK"))

		keyboard := tgbotapi.NewInlineKeyboardMarkup(topRow, bottomRow)

		var result string
		if repoBuild.LastBuildStatus == 0 {
			result = strBuildPassed
		} else {
			result = strBuildFailed
		}

		repoURL := fmt.Sprintf("%s%s", travisURL, repoBuild.Slug)
		msg := fmt.Sprintf(msgRepoBuild, repoBuild.Slug, repoBuild.LastBuildNumber,
			result, repoBuild.LastBuildStartedAt.Format("2006-01-02 15:04"),
			repoBuild.LastBuildFinishedAt.Sub(repoBuild.LastBuildStartedAt), repoURL)

		edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, msg)
		edit.ParseMode = tgbotapi.ModeMarkdown
		edit.ReplyMarkup = &keyboard
		bot.Send(edit)
	}

	bot.AnswerCallbackQuery(tgbotapi.CallbackConfig{CallbackQueryID: cq.ID})
}

func getRepoListPage(page int, c int64) ([]*travisRepo, bool) {
	conn := redisPool.Get()
	defer conn.Close()

	repos := getUserReposFromDb(&conn, c)
	if repos == nil {
		return nil, false
	} else if len(repos) == 0 {
		return nil, false
	}

	listRepos := []*travisRepo{}
	for i := page * 6; i < (page+1)*6; i++ {
		repo := repos[i]
		listRepos = append(listRepos, repo)

		if i == len(repos)-1 {
			break
		}
	}

	hasMore := false
	if len(repos)-(page+1)*6 > 0 {
		hasMore = true
	}

	return listRepos, hasMore
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
			repoInfo := getRepoFromDb(&conn, repo)

			repoBuild := getRepoInfosID(repoInfo.ID)

			if repoBuild.LastBuildFinishedAt.IsZero() {
				log.WithFields(log.Fields{
					"repo": repo,
				}).Info("Travis build is running, checking back later")
				continue
			}

			reply, err := conn.Do("SMEMBERS", fmt.Sprintf(dbKeyRepoUsers, repo))
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
	dataNextRegex = regexp.MustCompile("NEXT[0-9]*")
	dataPrevRegex = regexp.MustCompile("PREV[0-9]*")
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
		if update.Message == nil && update.CallbackQuery == nil {
			continue
		}

		if update.CallbackQuery != nil {
			go handleCallbackQuery(&update)
			continue
		}

		go handleMessage(update.Message)
	}
}
