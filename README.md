# Telegram Reminder Bot

A telegram reminder bot which reserves messages for certain times, using Google Gemini API.

Reserve messages with natural language like:

```
"Tell me to turn the light off 5 minutes later."

"Send me this message at 09:00 tomorrow."

"한 달 뒤 다 때려치우라고 해줘"

... etc.
```

## Prerequisites

* A [Google API key](https://aistudio.google.com/app/apikey), and
* a machine which can build and run golang applications.

Gemini 1.5 Pro or Flash model is needed for several features. (eg. system instruction, etc.)

## Configurations

Create a configuration file:

```bash
$ cp config.json.sample config.json
$ vi config.json
```

and set your values:

```json
{
  "google_generative_model": "gemini-2.5-flash",

  "allowed_telegram_users": ["user1", "user2"],
  "db_filepath": "/path/to/reminder-db.sqlite",
  "default_hour": 8,
  "verbose": false,

  "telegram_bot_token": "123456:abcdefghijklmnop-QRSTUVWXYZ7890",
  "google_ai_api_key": "abcdefg-987654321"
}
```

### Using Infisical

You can use [Infisical](https://infisical.com/) for retrieving your bot token and api key:

```json
{
  "google_generative_model": "gemini-2.5-flash",

  "allowed_telegram_users": ["user1", "user2"],
  "db_filepath": null,
  "default_hour": 8,
  "verbose": false,

  "infisical": {
    "client_id": "012345-abcdefg-987654321",
    "client_secret": "aAbBcCdDeEfFgG0123456789xyzwXYZW",

    "project_id": "012345abcdefg",
    "environment": "dev",
    "secret_type": "shared",

    "telegram_bot_token_key_path": "/path/to/your/KEY_TO_REMINDER_BOT_TOKEN",
    "google_ai_api_key_key_path": "/path/to/your/KEY_TO_GOOGLE_API_KEY"
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

Create a systemd service file:

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

