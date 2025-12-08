// bot.go

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	gt "github.com/meinside/gemini-things-go"
	tg "github.com/meinside/telegram-bot-go"
)

const (
	intervalSeconds                = 1
	requestTimeoutSeconds          = 30
	ignorableRequestTimeoutSeconds = 3

	cmdStart         = `/start` // (internal)
	cmdStats         = `/stats`
	cmdHelp          = `/help`
	cmdCancel        = `/cancel`
	cmdLoad          = `/load` // (internal)
	cmdListReminders = `/list`
	cmdPrivacy       = `/privacy`

	msgStart                 = `This bot will reserve your messages and notify you at desired times, with ChatGPT API :-)`
	msgCmdNotSupported       = `Not a supported bot command: %s`
	msgTypeNotSupported      = `Not a supported message type.`
	msgDatabaseNotConfigured = `Database not configured. Set 'db_filepath' in your config file.`
	msgDatabaseEmpty         = `Database is empty.`
	msgHelp                  = `Help message here:

<b>/list</b>: list all the active reminders.
<b>/cancel</b>: cancel a reminder.
<b>/stats</b>: show stats of this bot.
<b>/privacy</b>: show privacy policy of this bot.
<b>/help</b>: show this help message.

<i>model: %s</i>
<i>version: %s</i>
<i>source code: <a href="%s">github</a></i>
`
	msgCommandCanceled        = `Command was canceled.`
	msgReminderCanceledFormat = `Reminder '%s' was canceled.`
	msgError                  = `An error has occurred.`
	msgResponseFormat         = `Will notify '%s' on %s.`
	msgSaveFailedFormat       = `Failed to save reminder '%s': %s`
	msgSelectWhat             = `Which time do you want for message: '%s'?`
	msgCancelWhat             = `Which one do you want to cancel?`
	msgCancel                 = `Cancel`
	msgParseFailedFormat      = `Failed to understand message: %s`
	msgListItemFormat         = `☑ %s; %s`
	msgNoReminders            = `There is no registered reminder.`
	msgNoClue                 = `There was no clue for the desired datetime in your message.`
	msgPrivacy                = "Privacy Policy:\n\n" + githubPageURL + `/raw/master/PRIVACY.md`

	systemInstruction = `You are a kind and considerate chat bot which is built for understanding user's prompt, extracting desired datetime and prompt from it, and sending the prompt at the exact datetime. Current datetime is '%s'.`

	// function call
	fnNameInferDatetime              = `infer_datetime`
	fnDescriptionInferDatetime       = `This function infers a datetime and a message from the original prompt text.`
	fnArgNameInferredDatetime        = `inferred_datetime`
	fnArgDescriptionInferredDatetime = `Inferred datetime which is formatted as 'yyyy.mm.dd hh:MM TZ'(eg. 2024.12.25 15:00 KST). If the time cannot be inferred, fallback to %02d:00.`
	fnArgNameMessageToSend           = `message_to_send`
	fnArgDescriptionMessageToSend    = `Inferred message to be sent at 'inferred_datetime'. If it cannot be inferred, use the original prompt.`

	datetimeFormat = `2006.01.02 15:04 MST` // yyyy.mm.dd hh:MM TZ

	// default configs
	defaultMonitorIntervalSeconds  = 30
	defaultTelegramIntervalSeconds = 60
	defaultMaxNumTries             = 5
	defaultGenerativeModel         = "gemini-2.5-flash"

	githubPageURL = `https://github.com/meinside/telegram-reminder-bot`
)

var _location *time.Location

// launch bot with given parameters
func runBot(conf config) {
	var err error

	_location, _ = time.LoadLocation("Local")

	token := conf.TelegramBotToken
	apiKey := conf.GoogleAIAPIKey

	if token == nil || apiKey == nil {
		logErrorAndDie(nil, "`telegram_bot_token` and/or `google_ai_api_key` missing")
	}

	// telegram bot client
	bot := tg.NewClient(*token)

	// gemini things client
	gtc, err := gt.NewClient(
		*conf.GoogleAIAPIKey,
		gt.WithModel(conf.GoogleGenerativeModel),
	)
	if err != nil {
		logErrorAndDie(nil, "error initializing gemini-things client: %s", err)
	}
	defer func() { _ = gtc.Close() }()
	gtc.SetSystemInstructionFunc(func() string {
		return fmt.Sprintf(systemInstruction, datetimeToStr(time.Now()))
	})

	// background context
	ctx := context.Background()

	// open database
	var db *Database
	if db, err = OpenDatabase(conf.DBFilepath); err != nil {
		logErrorAndDie(nil, "failed to open database: %s", err)
	}

	// delete webhook before polling updates
	ctxDeleteWebhook, cancelDeleteWebhook := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
	defer cancelDeleteWebhook()
	_ = bot.DeleteWebhook(ctxDeleteWebhook, false)

	// get bot info
	ctxGetMe, cancelGetMe := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
	defer cancelGetMe()
	if b := bot.GetMe(ctxGetMe); b.Ok {
		logInfo("launching bot: %s", userName(b.Result))

		// monitor queue
		logInfo("starting monitoring queue...")
		go monitorQueue(
			ctx,
			time.NewTicker(time.Duration(conf.MonitorIntervalSeconds)*time.Second),
			bot,
			conf,
			db,
		)

		// set message handler
		bot.SetMessageHandler(func(b *tg.Bot, update tg.Update, message tg.Message, edited bool) {
			if !isAllowed(conf, update) {
				logDebug(conf, "message not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleMessage(ctx, b, conf, db, gtc, update, message)
		})

		// set callback query handler
		bot.SetCallbackQueryHandler(func(b *tg.Bot, update tg.Update, callbackQuery tg.CallbackQuery) {
			if !isAllowed(conf, update) {
				logDebug(conf, "callback query not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleCallbackQuery(ctx, b, db, callbackQuery)
		})

		// set command handlers
		bot.AddCommandHandler(cmdStart, startCommandHandler(ctx, conf, db))
		bot.AddCommandHandler(cmdListReminders, listRemindersCommandHandler(ctx, conf, db))
		bot.AddCommandHandler(cmdStats, statsCommandHandler(ctx, conf, db))
		bot.AddCommandHandler(cmdHelp, helpCommandHandler(ctx, conf, db))
		bot.AddCommandHandler(cmdCancel, cancelCommandHandler(ctx, conf, db))
		bot.AddCommandHandler(cmdPrivacy, privacyCommandHandler(ctx, conf, db))
		bot.SetNoMatchingCommandHandler(noSuchCommandHandler(ctx, conf, db))

		// poll updates
		bot.StartPollingUpdates(0, intervalSeconds, func(b *tg.Bot, update tg.Update, err error) {
			if err == nil {
				if !isAllowed(conf, update) {
					logDebug(conf, "not allowed: %s", userNameFromUpdate(update))
					return
				}

				// type not supported
				if message := messageFromUpdate(update); message != nil {
					send(ctx, b, conf, db, msgTypeNotSupported, message.Chat.ID, &message.MessageID)
				}
			} else {
				logError(db, "failed to fetch updates: %s", err)
			}
		})
	} else {
		logInfo("failed to get bot info: %s", *b.Description)
	}
}

// checks if given update is allowed or not
func isAllowed(conf config, update tg.Update) bool {
	var username string
	if update.HasMessage() && update.Message.From.Username != nil {
		username = *update.Message.From.Username
	} else if update.HasEditedMessage() && update.EditedMessage.From.Username != nil {
		username = *update.EditedMessage.From.Username
	} else if update.HasCallbackQuery() && update.CallbackQuery.From.Username != nil {
		username = *update.CallbackQuery.From.Username
	}

	return slices.Contains(conf.AllowedTelegramUsers, username)
}

// poll queue items periodically
func monitorQueue(
	ctx context.Context,
	monitor *time.Ticker,
	client *tg.Bot,
	conf config,
	db *Database,
) {
	for range monitor.C {
		processQueue(ctx, client, conf, db)
	}
}

// process queue item
func processQueue(
	ctx context.Context,
	client *tg.Bot,
	conf config,
	db *Database,
) {
	if queue, err := db.DeliverableQueueItems(conf.MaxNumTries); err == nil {
		logDebug(conf, "checking queue: %d items...", len(queue))

		for _, q := range queue {
			go func(q QueueItem) {
				message := q.Message

				// send it
				ctxSend, cancelSend := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
				defer cancelSend()
				sent := client.SendMessage(
					ctxSend,
					q.ChatID,
					message,
					tg.OptionsSendMessage{}.
						SetReplyMarkup(defaultReplyMarkup()).
						SetReplyParameters(tg.NewReplyParameters(q.MessageID)))

				if sent.Ok {
					// mark as delivered
					if _, err := db.MarkQueueItemAsDelivered(q.ChatID, q.ID); err != nil {
						logError(db, "failed to mark chat id: %d, queue id: %d (%s)", q.ChatID, q.ID, err)
					}
				} else {
					logError(db, "failed to send reminder: %s", *sent.Description)
				}

				// increase num tries
				if _, err := db.IncreaseNumTries(q.ChatID, q.ID); err != nil {
					logError(db, "failed to increase num tries for chat id: %d, queue id: %d (%s)", q.ChatID, q.ID, err)
				}
			}(q)
		}
	} else {
		logError(db, "failed to process queue: %s", err)
	}
}

// handle allowed message update from telegram bot api
func handleMessage(
	ctx context.Context,
	bot *tg.Bot,
	conf config,
	db *Database,
	gtc *gt.Client,
	update tg.Update,
	message tg.Message,
) {
	var msg string

	chatID := message.Chat.ID

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup())

	// 'is typing...'
	ctxAction, cancelAction := context.WithTimeout(ctx, ignorableRequestTimeoutSeconds*time.Second)
	defer cancelAction()
	_ = bot.SendChatAction(
		ctxAction,
		chatID,
		tg.ChatActionTyping,
		tg.OptionsSendChatAction{},
	)

	if message := messageFromUpdate(update); message != nil {
		options.SetReplyParameters(tg.NewReplyParameters(message.MessageID))

		if message.HasText() {
			txt := *message.Text
			if parsed, errs := parse(ctx, conf, db, gtc, *message, txt); len(parsed) > 0 {
				parsed = filterParsed(conf, parsed)

				if len(parsed) == 1 {
					what := parsed[0].Message
					when := parsed[0].When

					if _, err := db.Enqueue(chatID, message.MessageID, what, when); err == nil {
						msg = fmt.Sprintf(msgResponseFormat,
							what, // NOTE: not shorten it
							datetimeToStr(when),
						)
					} else {
						msg = fmt.Sprintf(msgSaveFailedFormat,
							shorten(what, 100), // NOTE: shorten it
							err,
						)
					}
				} else if len(parsed) > 0 {
					if _, err := db.SaveTemporaryMessage(chatID, message.MessageID, parsed[0].Message); err == nil {
						msg = fmt.Sprintf(msgSelectWhat,
							parsed[0].Message, // NOTE: not shorten it
						)

						// options for inline keyboards
						options.SetReplyMarkup(tg.NewInlineKeyboardMarkup(
							datetimeButtonsForCallbackQuery(parsed, chatID, message.MessageID),
						))
					} else {
						msg = msgError
					}
				} else {
					msg = msgNoClue
				}
			} else {
				msg = fmt.Sprintf(msgParseFailedFormat,
					errors.Join(errs...),
				)
			}
		} else {
			logInfo("no text in usable message from update.")

			msg = msgTypeNotSupported
		}
	} else {
		logInfo("no usable message from update.")

		msg = msgTypeNotSupported
	}

	// fallback message
	if len(msg) <= 0 {
		msg = msgError
	}

	// send message
	ctxSend, cancelSend := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
	defer cancelSend()
	if sent := bot.SendMessage(
		ctxSend,
		chatID,
		msg,
		options,
	); !sent.Ok {
		logError(db, "failed to send message: %s", *sent.Description)
	}
}

// handle allowed callback query from telegram bot api
func handleCallbackQuery(
	ctx context.Context,
	b *tg.Bot,
	db *Database,
	query tg.CallbackQuery,
) {
	data := *query.Data

	msg := msgError

	if strings.HasPrefix(data, cmdCancel) {
		if data == cmdCancel {
			msg = msgCommandCanceled
		} else {
			cancelParam := strings.TrimSpace(strings.Replace(data, cmdCancel, "", 1))
			if queueID, err := strconv.Atoi(cancelParam); err == nil {
				if item, err := db.GetQueueItem(query.Message.Chat.ID, int64(queueID)); err == nil {
					if _, err := db.DeleteQueueItem(query.Message.Chat.ID, int64(queueID)); err == nil {
						msg = fmt.Sprintf(msgReminderCanceledFormat,
							item.Message, // NOTE: not shorten it
						)
					} else {
						logError(db, "failed to delete reminder: %s", err)
					}
				} else {
					logError(db, "failed to get reminder: %s", err)
				}
			} else {
				logError(db, "unprocessable callback query: %s", data)
			}
		}
	} else if strings.HasPrefix(data, cmdLoad) {
		params := strings.Split(strings.TrimSpace(strings.Replace(data, cmdLoad, "", 1)), "/")

		if len(params) >= 3 {
			if chatID, err := strconv.ParseInt(params[0], 10, 64); err == nil {
				if messageID, err := strconv.ParseInt(params[1], 10, 64); err == nil {
					if saved, err := db.LoadTemporaryMessage(chatID, messageID); err == nil {
						if when, err := time.ParseInLocation(datetimeFormat, params[2], _location); err == nil {
							if _, err := db.Enqueue(chatID, messageID, saved.Message, when); err == nil {
								msg = fmt.Sprintf(msgResponseFormat,
									shorten(saved.Message, 160), // NOTE: shorten it
									datetimeToStr(when),
								)

								// delete temporary message
								if _, err := db.DeleteTemporaryMessage(chatID, messageID); err != nil {
									logError(db, "failed to delete temporary message: %s", err)
								}
							} else {
								msg = fmt.Sprintf(msgSaveFailedFormat,
									shorten(saved.Message, 160), // NOTE: shorten it
									err,
								)
							}
						} else {
							logError(db, "failed to parse time: %s", err)
						}
					} else {
						logError(db, "failed to load temporary message with chat id: %d, message id: %d", chatID, messageID)
					}
				} else {
					logError(db, "failed to convert message id: %s", err)
				}
			} else {
				logError(db, "failed to convert chat id: %s", err)
			}
		} else {
			logError(db, "malformed inline keyboard data: %s", data)
		}
	} else {
		logError(db, "unprocessable callback query: %s", data)
	}

	// answer callback query
	ctxAnswer, cancelAnswer := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
	defer cancelAnswer()
	if apiResult := b.AnswerCallbackQuery(
		ctxAnswer,
		query.ID,
		tg.OptionsAnswerCallbackQuery{}.
			SetText(msg),
	); apiResult.Ok {
		// edit message and remove inline keyboards
		ctxEdit, cancelEdit := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
		defer cancelEdit()
		options := tg.OptionsEditMessageText{}.
			SetIDs(query.Message.Chat.ID, query.Message.MessageID)
		if apiResult := b.EditMessageText(ctxEdit, msg, options); !apiResult.Ok {
			logError(db, "failed to edit message text: %s", *apiResult.Description)
		}
	} else {
		logError(db, "failed to answer callback query with %s: %s", prettify(query), *apiResult.Description)
	}
}

// send given message to the chat
func send(
	ctx context.Context,
	bot *tg.Bot,
	conf config,
	db *Database,
	message string,
	chatID int64,
	messageID *int64,
) {
	ctxAction, cancelAction := context.WithTimeout(ctx, ignorableRequestTimeoutSeconds*time.Second)
	defer cancelAction()
	_ = bot.SendChatAction(ctxAction, chatID, tg.ChatActionTyping, nil)

	logDebug(conf, "[verbose] sending message to chat(%d): '%s'", chatID, message)

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup()).
		SetParseMode(tg.ParseModeHTML)
	if messageID != nil {
		options.SetReplyParameters(tg.NewReplyParameters(*messageID))
	}

	ctxSend, cancelSend := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
	defer cancelSend()
	if res := bot.SendMessage(
		ctxSend,
		chatID,
		message,
		options,
	); !res.Ok {
		logError(db, "failed to send message: %s", *res.Description)
	}
}

// return a /start command handler
func startCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID

			send(ctx, b, conf, db, msgStart, chatID, nil)
		}
	}
}

// return a /list command handler
func listRemindersCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			var msg string
			chatID := message.Chat.ID

			if reminders, err := db.UndeliveredQueueItems(chatID); err == nil {
				if len(reminders) > 0 {
					format := fmt.Sprintf("%s\n", msgListItemFormat)
					for _, r := range reminders {
						msg += fmt.Sprintf(format,
							datetimeToStr(r.FireOn),
							shorten(r.Message, 100), // NOTE: shorten it
						)
					}
				} else {
					msg = msgNoReminders
				}
			} else {
				logError(db, "failed to process %s: %s", cmdListReminders, err)
			}

			// send message
			if len(msg) <= 0 {
				msg = msgError
			}
			send(ctx, b, conf, db, msg, chatID, nil)
		}
	}
}

// return a /cancel command handler
func cancelCommandHandler(ctx context.Context, conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			var msg string
			chatID := message.Chat.ID
			options := tg.OptionsSendMessage{}.
				SetReplyMarkup(defaultReplyMarkup())

			if reminders, err := db.UndeliveredQueueItems(chatID); err == nil {
				if len(reminders) > 0 {
					// inline keyboards
					keys := make(map[string]string)
					for _, r := range reminders {
						keys[fmt.Sprintf(msgListItemFormat,
							datetimeToStr(r.FireOn),
							shorten(r.Message, 100), // NOTE: shorten it
						)] = fmt.Sprintf("%s %d", cmdCancel, r.ID)
					}
					buttons := tg.NewInlineKeyboardButtonsAsRowsWithCallbackData(keys)

					// add a cancel button for canceling reminder
					buttons = append(buttons, []tg.InlineKeyboardButton{
						tg.NewInlineKeyboardButton(msgCancel).
							SetCallbackData(cmdCancel),
					})

					// options
					options.SetReplyMarkup(tg.NewInlineKeyboardMarkup(buttons))

					msg = msgCancelWhat
				} else {
					msg = msgNoReminders
				}
			} else {
				logError(db, "failed to process %s: %s", cmdCancel, err)
			}

			// send message
			if len(msg) <= 0 {
				msg = msgError
			}

			ctxSend, cancelSend := context.WithTimeout(ctx, requestTimeoutSeconds*time.Second)
			defer cancelSend()
			if sent := b.SendMessage(ctxSend, chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /privacy command handler
func privacyCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID

			send(ctx, b, conf, db, msgPrivacy, chatID, nil)
		}
	}
}

// return a /stats command handler
func statsCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("stats command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			var msg string
			if db == nil {
				msg = msgDatabaseNotConfigured
			} else {
				msg = db.Stats()
			}

			send(ctx, b, conf, db, msg, chatID, &messageID)
		}
	}
}

// return a /help command handler
func helpCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("help command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			send(ctx, b, conf, db, helpMessage(conf), chatID, &messageID)
		}
	}
}

// return a 'no such command' handler
func noSuchCommandHandler(
	ctx context.Context,
	conf config,
	db *Database,
) func(b *tg.Bot, update tg.Update, cmd, args string) {
	return func(b *tg.Bot, update tg.Update, cmd, args string) {
		if !isAllowed(conf, update) {
			log.Printf("command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			send(ctx, b, conf, db, fmt.Sprintf(msgCmdNotSupported, cmd), chatID, &messageID)
		}
	}
}
