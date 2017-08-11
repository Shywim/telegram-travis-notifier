package main

import (
	"encoding/json"
	"fmt"

	log "github.com/Sirupsen/logrus"
	"github.com/garyburd/redigo/redis"
)

func getRepoFromDb(c *redis.Conn, id int64) *travisRepo {
	repoInfo := new(travisRepo)

	reply, err := (*c).Do("GET", fmt.Sprintf(dbKeyRepo, id))
	data, err := redis.Bytes(reply, err)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"repo":  id,
		}).Error("Error getting repo infos from redis")
		return nil
	}

	json.Unmarshal(data, repoInfo)
	return repoInfo
}

func getUserReposFromDb(c *redis.Conn, cID int64) (repos []*travisRepo) {
	conn := (*c)
	reply, err := conn.Do("SMEMBERS", fmt.Sprintf(dbKeyUserRepos, cID))
	reposID, err := utilInt64s(reply, err)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
			"user":  cID,
		}).Error("Error getting user repos from redis")
	}

	for _, repoID := range reposID {
		repo := getRepoFromDb(c, repoID)
		if repo != nil {
			repos = append(repos, repo)
		}
	}

	return
}

func deleteRepoFromDb(c *redis.Conn, cID int64, rID int64) {
	conn := (*c)
	conn.Do("SREM", fmt.Sprintf(dbKeyUserRepos, cID), rID)
	conn.Do("DEL", fmt.Sprintf(dbKeyUserLastBuild, cID, rID))
}
