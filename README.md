# Travis notifier for Telegram

## Usage

Simply add the bot yo your telegram: t.me/TravisNotifierBot and you're off!

Use the `/help` command if you need to.

## Building

This project use [govendor] for dependencies.

After you clone this repository, you can just run `make vendor` to install govendor and pull dependencies. Choose between
`make bot` or `make install` then run the binary, passing your [telegram token][ttoken] with `--token` and the bot is ready!

**Note**: you need a redis server either locally or you can pass a different address with `--redis`.

 [govendor]: https://github.com/kardianos/govendor
 [ttoken]: https://core.telegram.org/bots#creating-a-new-bot
