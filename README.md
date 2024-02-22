# Telegram Reminder Bot

A telegram reminder bot which reserves messages for certain times, using ChatGPT API.

Reserve messages with natural language like:

```
"Tell me to turn the light off 5 minutes later."

"Send me this message at 09:00 tomorrow."

"한 달 뒤 다 때려치우라고 해줘"

... etc.
```

## Prerequisites

* A [paid OpenAI account](https://openai.com/pricing), and
* a machine which can build and run golang applications.

## Configurations

Create a configuration file:

```bash
$ cp config.json.sample config.json
$ vi config.json
```

and set your values:

```json
{
  "allowed_telegram_users": ["user1", "user2"],
  "openai_model": "gpt-3.5-turbo",
  "db_filepath": "/path/to/reminder-db.sqlite",
  "default_hour": 8,
  "verbose": false,

  "telegram_bot_token": "123456:abcdefghijklmnop-QRSTUVWXYZ7890",
  "openai_api_key": "key-ABCDEFGHIJK1234567890",
  "openai_org_id": "org-1234567890abcdefghijk"
}
```

### Using Infisical

You can use [Infisical](https://infisical.com/) for retrieving your bot token and api key:

```json
{
  "allowed_telegram_users": ["user1", "user2"],
  "openai_model": "gpt-3.5-turbo",
  "db_filepath": null,
  "default_hour": 8,
  "verbose": false,

  "infisical": {
    "client_id": "012345-abcdefg-987654321",
    "client_secret": "aAbBcCdDeEfFgG0123456789xyzwXYZW",

    "workspace_id": "012345abcdefg",
    "environment": "dev",
    "secret_type": "shared",

    "telegram_bot_token_key_path": "/path/to/your/KEY_TO_REMINDER_BOT_TOKEN",
    "openai_api_key_key_path": "/path/to/your/KEY_TO_OPENAI_API_KEY",
    "openai_org_id_key_path": "/path/to/your/KEY_TO_OPENAI_ORG_ID"
  }
}
```

## Build

```bash
$ go build
```

## Run

### Run the binary directly

Run the built binary file with the config file's path:

```bash
$ ./telegram-reminder-bot /path/to/config.json
```

### Or run it as a systemd service

Createa a systemd service file:

```
[Unit]
Description=Telegram Reminder Bot
After=syslog.target
After=network.target

[Service]
Type=simple
User=ubuntu
Group=ubuntu
WorkingDirectory=/dir/to/telegram-reminder-bot
ExecStart=/dir/to/telegram-reminder-bot/telegram-reminder-bot /path/to/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

and `systemctl` enable|start|restart|stop the service.

## Commands

- `/stats` for statistics of parsed/generated messages.
- `/cancel` for cancelling reserved messages.
- `/list` for listing reserved messages.
- `/help` for help message.

## Todo

- [ ] Optimize prompts.
- [ ] I18nalize bot messages.

## License

The MIT License (MIT)

Copyright © 2023 Sungjin Han

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

