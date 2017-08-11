PACKAGE = github.com/Shywim/telegram-travis-notifier

# enable user to use different go executable (e.g. different version...)
GOEXE ?= go

.PHONY: vendor help
.DEFAULT_GOAL := help

vendor: ## Install govendor and sync the dependencies
	${GOEXE} get github.com/kardianos/govendor
	govendor sync

bot: vendor ## Build binary
	${GOEXE} build ${PACKAGE}

install: vendor ## Install bot binary
	${GOEXE} install ${PACKAGE}

help: ## This help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
