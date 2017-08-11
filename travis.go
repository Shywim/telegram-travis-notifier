package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	log "github.com/Sirupsen/logrus"
)

func utilTravisURL(s string) string {
	return fmt.Sprintf("%s%s", travisURL, s)
}

func getRepoInfos(url string) *travisRepo {
	resp, err := http.Get(url)
	if err != nil {
		log.WithError(err).Error("Failed to fetch repo data")
		return nil
	}

	repoInfos := newTravisRepo()

	body, err := ioutil.ReadAll(resp.Body)
	err = json.Unmarshal(body, repoInfos)
	if err != nil {
		log.WithError(err).Error("Failed to fetch repo data")
		return nil
	}

	return repoInfos
}

func getRepoInfosID(id int64) *travisRepo {
	return getRepoInfos(fmt.Sprintf("%s%d.json", travisURLApi, id))
}

func getRepoInfosName(repo string) *travisRepo {
	return getRepoInfos(fmt.Sprintf("%s%s.json", travisURLApi, repo))
}

func checkRepoExists(repo string) (bool, string) {
	resp, err := http.Get(fmt.Sprintf("%s%s", travisURLApi, repo))
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
