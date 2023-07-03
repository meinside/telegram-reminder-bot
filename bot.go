package main

// bot.go

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/meinside/geektoken"
	"github.com/meinside/openai-go"
	tg "github.com/meinside/telegram-bot-go"
	"github.com/meinside/version-go"
)

const (
	intervalSeconds = 1

	cmdStart         = "/start" // (internal)
	cmdCount         = "/count"
	cmdStats         = "/stats"
	cmdHelp          = "/help"
	cmdCancel        = "/cancel"
	cmdLoad          = "/load" // (internal)
	cmdListReminders = "/list"

	msgStart                 = "This bot will reserve your messages and notify you at desired times, with ChatGPT API :-)"
	msgCmdNotSupported       = "Not a supported bot command: %s"
	msgTypeNotSupported      = "Not a supported message type."
	msgDatabaseNotConfigured = "Database not configured. Set `db_filepath` in your config file."
	msgDatabaseEmpty         = "Database is empty."
	msgTokenCount            = "<b>%d</b> tokens in <b>%d</b> chars <i>(cl100k_base)</i>"
	msgHelp                  = `Help message here:

/list : list all the active reminders.
/cancel : cancel a reminder.
/count [some_text] : count the number of tokens in a given text.
/stats : show stats of this bot.
/help : show this help message.

<i>version: %s</i>
`
	msgCommandCanceled        = "Command was canceled."
	msgReminderCanceledFormat = "Reminder was canceled: %s"
	msgError                  = "An error has occurred."
	msgResponseFormat         = `Will notify "%s" on %s.`
	msgSaveFailedFormat       = "Failed to save reminder: %s (%s)"
	msgSelectWhat             = "Which one do you want to select?"
	msgCancelWhat             = "Which one do you want to cancel?"
	msgCancel                 = "Cancel"
	msgParseFailedFormat      = "Failed to understand message: %s"
	msgListItemFormat         = "☑ %s; %s"
	msgNoReminders            = "There is no registered reminder."
	msgNoClue                 = "There was no clue for the time in the message."

	datetimeFormat = "2006.01.02 15:04" // yyyy.mm.dd hh:MM

	funcNameReserveMessageAtAbsoluteTime = "reserve_message_absolute"

	// default configs
	defaultMonitorIntervalSeconds  = 30
	defaultTelegramIntervalSeconds = 60
	defaultMaxNumTries             = 5
	defaultChatCompletionModel     = "gpt-3.5-turbo"
)

var _location *time.Location

// config struct for loading a configuration file
type config struct {
	// telegram bot api
	TelegramBotToken string `json:"telegram_bot_token"`

	MonitorIntervalSeconds  int `json:"monitor_interval_seconds"`
	TelegramIntervalSeconds int `json:"telegram_interval_seconds"`
	MaxNumTries             int `json:"max_num_tries"`

	// openai api
	OpenAIAPIKey         string `json:"openai_api_key"`
	OpenAIOrganizationID string `json:"openai_org_id"`
	OpenAIModel          string `json:"openai_model,omitempty"`

	// database logging
	DBFilepath string `json:"db_filepath"`

	// other configurations
	AllowedTelegramUsers []string `json:"allowed_telegram_users"`
	DefaultHour          int      `json:"default_hour,omitempty"`
	Verbose              bool     `json:"verbose,omitempty"`
}

// function arguments
type arguments struct {
	Message string `json:"message"`
	When    string `json:"when"`
}

// load config at given path
func loadConfig(fpath string) (conf config, err error) {
	var bytes []byte
	if bytes, err = os.ReadFile(fpath); err == nil {
		if err = json.Unmarshal(bytes, &conf); err == nil {
			// check config values
			if conf.MonitorIntervalSeconds <= 0 {
				conf.MonitorIntervalSeconds = defaultMonitorIntervalSeconds
			}
			if conf.TelegramIntervalSeconds <= 0 {
				conf.TelegramIntervalSeconds = defaultTelegramIntervalSeconds
			}
			if conf.MaxNumTries <= 0 {
				conf.MaxNumTries = defaultMaxNumTries
			}
			if conf.OpenAIModel == "" {
				conf.OpenAIModel = defaultChatCompletionModel
			}
			if conf.DefaultHour < 0 {
				conf.DefaultHour = 0
			} else if conf.DefaultHour >= 24 {
				conf.DefaultHour = 23
			}

			return conf, nil
		}
	}

	return config{}, err
}

// launch bot with given parameters
func runBot(conf config) {
	var err error

	_location, _ = time.LoadLocation("Local")

	token := conf.TelegramBotToken
	apiKey := conf.OpenAIAPIKey
	orgID := conf.OpenAIOrganizationID

	bot := tg.NewClient(token)
	client := openai.NewClient(apiKey, orgID)

	// set verbosity
	client.Verbose = conf.Verbose

	// open database
	var db *Database
	if db, err = OpenDatabase(conf.DBFilepath); err != nil {
		logErrorAndDie(nil, "failed to open database: %s", err)
	}

	_ = bot.DeleteWebhook(false) // delete webhook before polling updates
	if b := bot.GetMe(); b.Ok {
		logInfo("launching bot: %s", userName(b.Result))

		// monitor queue
		logInfo("starting monitoring queue...")
		go monitorQueue(
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

			handleMessage(b, client, conf, db, update, message)
		})

		// set callback query handler
		bot.SetCallbackQueryHandler(func(b *tg.Bot, update tg.Update, callbackQuery tg.CallbackQuery) {
			if !isAllowed(conf, update) {
				logDebug(conf, "callback query not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleCallbackQuery(b, client, conf, db, update, callbackQuery)
		})

		// set command handlers
		bot.AddCommandHandler(cmdStart, startCommandHandler(conf, db))
		bot.AddCommandHandler(cmdListReminders, listRemindersCommandHandler(conf, db))
		bot.AddCommandHandler(cmdStats, statsCommandHandler(conf, db))
		bot.AddCommandHandler(cmdHelp, helpCommandHandler(conf, db))
		bot.AddCommandHandler(cmdCount, countCommandHandler(conf, db))
		bot.AddCommandHandler(cmdCancel, cancelCommandHandler(conf, db))
		bot.SetNoMatchingCommandHandler(noSuchCommandHandler(conf, db))

		// poll updates
		bot.StartPollingUpdates(0, intervalSeconds, func(b *tg.Bot, update tg.Update, err error) {
			if err == nil {
				if !isAllowed(conf, update) {
					logDebug(conf, "not allowed: %s", userNameFromUpdate(update))
					return
				}

				// type not supported
				message := messageFromUpdate(update)
				if message != nil {
					send(b, conf, db, msgTypeNotSupported, message.Chat.ID, &message.MessageID)
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

	for _, allowedUser := range conf.AllowedTelegramUsers {
		if allowedUser == username {
			return true
		}
	}

	return false
}

// poll queue items periodically
func monitorQueue(monitor *time.Ticker, client *tg.Bot, conf config, db *Database) {
	for range monitor.C {
		processQueue(client, conf, db)
	}
}

// process queue item
func processQueue(client *tg.Bot, conf config, db *Database) {
	if queue, err := db.DeliverableQueueItems(conf.MaxNumTries); err == nil {
		logDebug(conf, "checking queue: %d items...", len(queue))

		for _, q := range queue {
			go func(q QueueItem) {
				message := q.Message

				// send it
				sent := client.SendMessage(
					q.ChatID,
					message,
					tg.OptionsSendMessage{}.
						SetReplyMarkup(defaultReplyMarkup()).
						SetReplyToMessageID(q.MessageID))

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
func handleMessage(bot *tg.Bot, client *openai.Client, conf config, db *Database, update tg.Update, message tg.Message) {
	var msg string

	chatID := message.Chat.ID

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup())

	// 'is typing...'
	bot.SendChatAction(chatID, tg.ChatActionTyping, tg.OptionsSendChatAction{})

	if message := messageFromUpdate(update); message != nil {
		if message.HasText() {
			txt := *message.Text
			if parsed, errs := parse(client, conf, db, *message, txt); len(errs) == 0 {
				parsed = filterParsed(conf, parsed)

				if len(parsed) == 1 {
					what := parsed[0].Message
					when := parsed[0].When

					if _, err := db.Enqueue(chatID, message.MessageID, what, when); err == nil {
						msg = fmt.Sprintf(msgResponseFormat,
							what,
							datetimeToStr(when),
						)
					} else {
						msg = fmt.Sprintf(msgSaveFailedFormat, what, err)
					}
				} else if len(parsed) > 0 {
					if _, err := db.SaveTemporaryMessage(chatID, message.MessageID, parsed[0].Message); err == nil {
						msg = msgSelectWhat

						// options for inline keyboards
						options.SetReplyMarkup(tg.InlineKeyboardMarkup{
							InlineKeyboard: datetimeButtonsForCallbackQuery(parsed, chatID, message.MessageID),
						})
					} else {
						msg = msgError
					}
				} else {
					msg = msgNoClue
				}
			} else {
				msg = fmt.Sprintf(msgParseFailedFormat, errors.Join(errs...))
			}
		} else {
			logInfo("no text in usable message from update.")

			msg = msgTypeNotSupported
		}
	} else {
		logInfo("no usable message from update.")

		msg = msgTypeNotSupported
	}

	// send message
	if len(msg) <= 0 {
		msg = msgError
	}
	if sent := bot.SendMessage(chatID, msg, options); !sent.Ok {
		logError(db, "failed to send message: %s", *sent.Description)
	}
}

// handle allowed callback query from telegram bot api
func handleCallbackQuery(b *tg.Bot, client *openai.Client, conf config, db *Database, update tg.Update, query tg.CallbackQuery) {
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
						msg = fmt.Sprintf(msgReminderCanceledFormat, item.Message)
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
									saved.Message,
									datetimeToStr(when),
								)

								// delete temporary message
								if _, err := db.DeleteTemporaryMessage(chatID, messageID); err != nil {
									logError(db, "failed to delete temporary message: %s", err)
								}
							} else {
								msg = fmt.Sprintf(msgSaveFailedFormat, saved.Message, err)
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
	if apiResult := b.AnswerCallbackQuery(query.ID, map[string]interface{}{"text": msg}); apiResult.Ok {
		// edit message and remove inline keyboards
		options := map[string]interface{}{
			"chat_id":    query.Message.Chat.ID,
			"message_id": query.Message.MessageID,
		}
		if apiResult := b.EditMessageText(msg, options); !apiResult.Ok {
			logError(db, "failed to edit message text: %s", *apiResult.Description)
		}
	} else {
		logError(db, "failed to answer callback query: %+v", query)
	}
}

// get usable message from given update
func messageFromUpdate(update tg.Update) (message *tg.Message) {
	if update.HasMessage() && update.Message.HasText() {
		message = update.Message
	} else if update.HasMessage() && update.Message.HasDocument() {
		message = update.Message
	} else if update.HasEditedMessage() && update.EditedMessage.HasText() {
		message = update.EditedMessage
	}

	if message == nil {
		logInfo("no usable message from update.")
	}

	return message
}

// send given message to the chat
func send(bot *tg.Bot, conf config, db *Database, message string, chatID int64, messageID *int64) {
	_ = bot.SendChatAction(chatID, tg.ChatActionTyping, nil)

	logDebug(conf, "[verbose] sending message to chat(%d): '%s'", chatID, message)

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup()).
		SetParseMode(tg.ParseModeHTML)
	if messageID != nil {
		options.SetReplyToMessageID(*messageID)
	}
	if res := bot.SendMessage(chatID, message, options); !res.Ok {
		logError(db, "failed to send message: %s", *res.Description)
	}
}

// type for parsed items
type parsedItem struct {
	Message   string
	When      time.Time
	Generated bool
}

// parse given string and return the function call
func parse(client *openai.Client, conf config, db *Database, message tg.Message, text string) (result []parsedItem, errs []error) {
	result = []parsedItem{}
	errs = []error{}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := userName(message.From)

	if created, err := client.CreateChatCompletion(conf.OpenAIModel,
		[]openai.ChatMessage{
			openai.NewChatAssistantMessage(fmt.Sprintf("Current time is %s.", datetimeToStr(time.Now()))),
			openai.NewChatUserMessage(text),
		},
		openai.ChatCompletionOptions{}.
			SetUser(userAgent(userID)).
			SetFunctions([]openai.ChatCompletionFunction{
				openai.NewChatCompletionFunction(
					funcNameReserveMessageAtAbsoluteTime,
					"Reserve given message and send it back at given absolute time.",
					openai.NewChatCompletionFunctionParameters().
						AddPropertyWithDescription("message", "string", "The message to reserve.").
						AddPropertyWithDescription("when", "string", "The time when the reserved message should be sent back, in format: 'yyyy.mm.dd hh:MM'.").
						SetRequiredParameters([]string{"message", "when"}), // yyyy.mm.dd hh:MM
				),
			}).
			SetFunctionCall(openai.ChatCompletionFunctionCallAuto)); err != nil {
		errs = append(errs, fmt.Errorf("failed to create chat completion: %s", err))

		// log failure
		savePromptAndResult(db, chatID, userID, username, text, created.Usage.PromptTokens, "", "", created.Usage.CompletionTokens, false)

		logError(db, "failed to create chat completion: %s", err)
	} else {
		if len(created.Choices) <= 0 {
			logError(db, "there was no returned choice")
		} else {
			for _, choice := range created.Choices {
				message := choice.Message

				if message.FunctionCall == nil {
					logError(db, "there was no returned function call")
				} else {
					functionName := message.FunctionCall.Name
					if functionName == "" {
						logError(db, "there was no returned function call name")
					}

					if message.FunctionCall.Arguments == nil {
						logError(db, "there were no returned function call arguments")
					} else {
						if functionName == funcNameReserveMessageAtAbsoluteTime {
							var args arguments
							if err := message.FunctionCall.ArgumentsInto(&args); err == nil {
								if args.Message != "" && args.When != "" {
									if t, err := time.ParseInLocation(datetimeFormat, args.When, _location); err == nil {
										result = append(result, parsedItem{
											Message:   args.Message,
											When:      t,
											Generated: false,
										})
									} else {
										errs = append(errs, fmt.Errorf("failed to parse time: %s for 'when' argument", err))

										logError(db, "failed to parse time: %s for 'when' argument", errors.Join(errs...))
									}
								} else {
									logError(db, "values in arguments were not valid: %+v", map[string]string{
										"message": args.Message,
										"when":    args.When,
									})
								}
							} else {
								logError(db, "failed to parse arguments: %s", err)
							}
						}

						// log success
						functionArgs := ""
						if message.FunctionCall.Arguments != nil {
							functionArgs = *message.FunctionCall.Arguments
						}
						savePromptAndResult(db, chatID, userID, username, text, created.Usage.PromptTokens, functionName, functionArgs, created.Usage.CompletionTokens, true)
					}
				}
			}
		}
	}

	return result, errs
}

// filter parsed items to be all valid
func filterParsed(conf config, parsed []parsedItem) (filtered []parsedItem) {
	// add some generated items for convenience
	generated := []parsedItem{}
	for _, p := range parsed {
		// save it as it is,
		generated = append(generated, p)

		// and add generated ones,
		when := p.When.In(_location)
		hour, minute := when.Hour(), when.Minute()
		if hour == 0 && minute == 0 {
			// default hour
			generated = append(generated, parsedItem{
				Message:   p.Message,
				When:      p.When.In(_location).Add(time.Hour * time.Duration(conf.DefaultHour)),
				Generated: true,
			})
		} else if hour < 12 {
			// add 12 hours if it is AM
			generated = append(generated, parsedItem{
				Message:   p.Message,
				When:      p.When.In(_location).Add(time.Hour * 12),
				Generated: true,
			})
		}
	}

	// remove already-passed or duplicated ones
	filtered = []parsedItem{}
	duplicated := map[string]bool{}
	now := time.Now()
	for _, p := range generated {
		when := p.When.In(_location)

		// remove duplicated ones,
		dup := when.Format(datetimeFormat)
		if _, exists := duplicated[dup]; exists {
			continue
		} else {
			duplicated[dup] = true // mark as duplicated,

			// and remove already-passed ones
			if when.After(now) {
				filtered = append(filtered, p)
			}
		}
	}

	return filtered
}

// generate a user-agent value
func userAgent(userID int64) string {
	return fmt.Sprintf("telegram-reminder-bot:%d", userID)
}

// generate user's name
func userName(user *tg.User) string {
	if user.Username != nil {
		return fmt.Sprintf("@%s (%s)", *user.Username, user.FirstName)
	} else {
		return user.FirstName
	}
}

// generate user's name from update
func userNameFromUpdate(update tg.Update) string {
	if user := update.GetFrom(); user != nil {
		return userName(user)
	}

	logInfo("there was no `from` in `update`")

	return "unknown"
}

var _tokenizer *geektoken.Tokenizer = nil

// count BPE tokens for given `text`
func countTokens(text string) (result int, err error) {
	result = 0

	// lazy-load the tokenizer
	if _tokenizer == nil {
		var tokenizer geektoken.Tokenizer
		tokenizer, err = geektoken.GetTokenizerWithEncoding(geektoken.EncodingCl100kBase)

		if err == nil {
			_tokenizer = &tokenizer
		}
	}

	if _tokenizer == nil {
		return 0, fmt.Errorf("tokenizer is not initialized.")
	}

	var tokens []int
	tokens, err = _tokenizer.Encode(text, nil, nil)

	if err == nil {
		return len(tokens), nil
	}

	return result, err
}

// save prompt and its result to logs database
func savePromptAndResult(db *Database, chatID, userID int64, username string, prompt string, promptTokens int, functionName, functionArgs string, resultTokens int, resultSuccessful bool) {
	if db != nil {
		if err := db.SavePrompt(Prompt{
			ChatID:   chatID,
			UserID:   userID,
			Username: username,
			Text:     prompt,
			Tokens:   promptTokens,
			Result: ParsedItem{
				Successful:   resultSuccessful,
				FunctionName: functionName,
				FunctionArgs: functionArgs,
				Tokens:       resultTokens,
			},
		}); err != nil {
			log.Printf("failed to save prompt & result to database: %s", err)
		}
	}
}

// generate a help message with version info
func helpMessage() string {
	return fmt.Sprintf(msgHelp, version.Build(version.OS|version.Architecture|version.Revision))
}

// return a /start command handler
func startCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("start command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID

			send(b, conf, db, msgStart, chatID, nil)
		}
	}
}

// return a /list command handler
func listRemindersCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
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
					format := fmt.Sprintf("%s\n", msgListItemFormat)
					for _, r := range reminders {
						msg += fmt.Sprintf(format, datetimeToStr(r.FireOn), r.Message)
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
			if sent := b.SendMessage(chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /cancel command handler
func cancelCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
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
						keys[fmt.Sprintf(msgListItemFormat, datetimeToStr(r.FireOn), r.Message)] = fmt.Sprintf("%s %d", cmdCancel, r.ID)
					}
					buttons := tg.NewInlineKeyboardButtonsAsRowsWithCallbackData(keys)

					// add a cancel button for canceling reminder
					cancel := cmdCancel
					buttons = append(buttons, []tg.InlineKeyboardButton{
						{
							Text:         msgCancel,
							CallbackData: &cancel,
						},
					})

					// options
					options.SetReplyMarkup(tg.InlineKeyboardMarkup{
						InlineKeyboard: buttons,
					})

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
			if sent := b.SendMessage(chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /stats command handler
func statsCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
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

			send(b, conf, db, msg, chatID, &messageID)
		}
	}
}

// return a /help command handler
func helpCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, _ string) {
		if !isAllowed(conf, update) {
			log.Printf("help command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			send(b, conf, db, helpMessage(), chatID, &messageID)
		}
	}
}

// return a /count command handler
func countCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if !isAllowed(conf, update) {
			log.Printf("count command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			var msg string
			if count, err := countTokens(args); err == nil {
				msg = fmt.Sprintf(msgTokenCount, count, len(args))
			} else {
				msg = err.Error()
			}

			send(b, conf, db, msg, chatID, &messageID)
		}
	}
}

// return a 'no such command' handler
func noSuchCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, cmd, args string) {
	return func(b *tg.Bot, update tg.Update, cmd, args string) {
		if !isAllowed(conf, update) {
			log.Printf("command not allowed: %s", userNameFromUpdate(update))
			return
		}

		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID
			messageID := message.MessageID

			msg := fmt.Sprintf(msgCmdNotSupported, cmd)
			send(b, conf, db, msg, chatID, &messageID)
		}
	}
}

var _stdout = log.New(os.Stdout, "", log.LstdFlags)
var _stderr = log.New(os.Stderr, "", log.LstdFlags)

// log info message
func logInfo(format string, a ...any) {
	_stdout.Printf(format, a...)
}

// log debug message (printed to stdout only when `IsVerbose` is true)
func logDebug(conf config, format string, a ...any) {
	if conf.Verbose {
		_stdout.Printf(format, a...)
	}
}

// log error message
func logError(db *Database, format string, a ...any) {
	if db != nil {
		db.LogError(format, a...)
	}

	_stderr.Printf(format, a...)
}

// log error message and exit(1)
func logErrorAndDie(db *Database, format string, a ...any) {
	if db != nil {
		db.LogError(format, a...)
	}

	_stderr.Fatalf(format, a...)
}

// default reply markup
func defaultReplyMarkup() tg.ReplyKeyboardMarkup {
	return tg.ReplyKeyboardMarkup{ // show keyboards
		Keyboard: [][]tg.KeyboardButton{
			tg.NewKeyboardButtons(cmdListReminders, cmdCancel, cmdStats, cmdHelp),
		},
		ResizeKeyboard: true,
	}
}

// generate inline keyboard buttons for multiple datetimes
func datetimeButtonsForCallbackQuery(items []parsedItem, chatID int64, messageID int64) [][]tg.InlineKeyboardButton {
	// datetime buttons
	keys := make(map[string]string)

	var title, generated string
	for _, item := range items {
		if item.Generated {
			generated = " *"
		} else {
			generated = ""
		}
		title = fmt.Sprintf("%s%s", datetimeToStr(item.When), generated)
		keys[title] = fmt.Sprintf("%s %d/%d/%s", cmdLoad, chatID, messageID, datetimeToStr(item.When))
	}
	buttons := tg.NewInlineKeyboardButtonsAsRowsWithCallbackData(keys)

	// add cancel button
	cancel := cmdCancel
	buttons = append(buttons, []tg.InlineKeyboardButton{
		{
			Text:         msgCancel,
			CallbackData: &cancel,
		},
	})

	return buttons
}

// format given time to string
func datetimeToStr(t time.Time) string {
	return t.In(_location).Format(datetimeFormat)
}
