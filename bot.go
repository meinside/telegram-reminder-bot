package main

// bot.go

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	// infisical
	infisical "github.com/infisical/go-sdk"
	"github.com/infisical/go-sdk/packages/models"

	// google ai
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/googleapi"

	// my libraries
	gt "github.com/meinside/gemini-things-go"
	tg "github.com/meinside/telegram-bot-go"
	"github.com/meinside/version-go"

	// others
	"github.com/tailscale/hujson"
)

const (
	intervalSeconds = 1

	cmdStart         = "/start" // (internal)
	cmdStats         = "/stats"
	cmdHelp          = "/help"
	cmdCancel        = "/cancel"
	cmdLoad          = "/load" // (internal)
	cmdListReminders = "/list"
	cmdPrivacy       = "/privacy"

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

	systemInstruction = `You are a kind and considerate chat bot which is built for understanding user's prompt, extracting desired datetime and promt from it, and sending the prompt at the exact datetime. Current datetime is '%s'.`

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
	//defaultGenerativeModel = "gemini-1.5-pro-latest"
	defaultGenerativeModel = "gemini-1.5-flash-latest"

	githubPageURL = `https://github.com/meinside/telegram-reminder-bot`
)

var _location *time.Location

// config struct for loading a configuration file
type config struct {
	GoogleGenerativeModel string `json:"google_generative_model,omitempty"`

	MonitorIntervalSeconds  int    `json:"monitor_interval_seconds"`
	TelegramIntervalSeconds int    `json:"telegram_interval_seconds"`
	MaxNumTries             int    `json:"max_num_tries"`
	DBFilepath              string `json:"db_filepath"`

	// other optional configurations
	AllowedTelegramUsers []string `json:"allowed_telegram_users"`
	DefaultHour          int      `json:"default_hour,omitempty"`
	Verbose              bool     `json:"verbose,omitempty"`

	// token and api key
	TelegramBotToken *string `json:"telegram_bot_token,omitempty"`
	GoogleAIAPIKey   *string `json:"google_ai_api_key,omitempty"`

	// or Infisical settings
	Infisical *struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`

		ProjectID   string `json:"project_id"`
		Environment string `json:"environment"`
		SecretType  string `json:"secret_type"`

		TelegramBotTokenKeyPath string `json:"telegram_bot_token_key_path"`
		GoogleAIAPIKeyKeyPath   string `json:"google_ai_api_key_key_path"`
	} `json:"infisical,omitempty"`
}

// load config at given path
func loadConfig(fpath string) (conf config, err error) {
	var bytes []byte
	if bytes, err = os.ReadFile(fpath); err == nil {
		if bytes, err = standardizeJSON(bytes); err == nil {
			if err = json.Unmarshal(bytes, &conf); err == nil {
				if (conf.TelegramBotToken == nil || conf.GoogleAIAPIKey == nil) &&
					conf.Infisical != nil {
					// read token and api key from infisical
					client := infisical.NewInfisicalClient(context.TODO(), infisical.Config{
						SiteUrl: "https://app.infisical.com",
					})

					_, err = client.Auth().UniversalAuthLogin(conf.Infisical.ClientID, conf.Infisical.ClientSecret)
					if err != nil {
						return config{}, fmt.Errorf("failed to authenticate with Infisical: %s", err)
					}

					var keyPath string
					var secret models.Secret

					// telegram bot token
					keyPath = conf.Infisical.TelegramBotTokenKeyPath
					secret, err = client.Secrets().Retrieve(infisical.RetrieveSecretOptions{
						ProjectID:   conf.Infisical.ProjectID,
						Type:        conf.Infisical.SecretType,
						Environment: conf.Infisical.Environment,
						SecretPath:  path.Dir(keyPath),
						SecretKey:   path.Base(keyPath),
					})
					if err == nil {
						val := secret.SecretValue
						conf.TelegramBotToken = &val
					} else {
						return config{}, fmt.Errorf("failed to retrieve `telegram_bot_token` from Infisical: %s", err)
					}

					// google ai api key
					keyPath = conf.Infisical.GoogleAIAPIKeyKeyPath
					secret, err = client.Secrets().Retrieve(infisical.RetrieveSecretOptions{
						ProjectID:   conf.Infisical.ProjectID,
						Type:        conf.Infisical.SecretType,
						Environment: conf.Infisical.Environment,
						SecretPath:  path.Dir(keyPath),
						SecretKey:   path.Base(keyPath),
					})
					if err == nil {
						val := secret.SecretValue
						conf.GoogleAIAPIKey = &val
					} else {
						return config{}, fmt.Errorf("failed to retrieve `google_ai_api_key` from Infisical: %s", err)
					}
				}

				// set default/fallback values
				if conf.MonitorIntervalSeconds <= 0 {
					conf.MonitorIntervalSeconds = defaultMonitorIntervalSeconds
				}
				if conf.TelegramIntervalSeconds <= 0 {
					conf.TelegramIntervalSeconds = defaultTelegramIntervalSeconds
				}
				if conf.MaxNumTries <= 0 {
					conf.MaxNumTries = defaultMaxNumTries
				}
				if conf.GoogleGenerativeModel == "" {
					conf.GoogleGenerativeModel = defaultGenerativeModel
				}
				if conf.DefaultHour < 0 || conf.DefaultHour >= 24 {
					conf.DefaultHour = 0
				}
			}
		}
	}

	return conf, err
}

// standardize given JSON (JWCC) bytes
func standardizeJSON(b []byte) ([]byte, error) {
	ast, err := hujson.Parse(b)
	if err != nil {
		return b, err
	}
	ast.Standardize()

	return ast.Pack(), nil
}

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
	gtc, err := gt.NewClient(conf.GoogleGenerativeModel, *conf.GoogleAIAPIKey)
	if err != nil {
		logErrorAndDie(nil, "error initializing gemini-things client: %s", err)
	}
	defer gtc.Close()
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

			handleMessage(ctx, b, conf, db, gtc, update, message)
		})

		// set callback query handler
		bot.SetCallbackQueryHandler(func(b *tg.Bot, update tg.Update, callbackQuery tg.CallbackQuery) {
			if !isAllowed(conf, update) {
				logDebug(conf, "callback query not allowed: %s", userNameFromUpdate(update))
				return
			}

			handleCallbackQuery(b, db, callbackQuery)
		})

		// set command handlers
		bot.AddCommandHandler(cmdStart, startCommandHandler(conf, db))
		bot.AddCommandHandler(cmdListReminders, listRemindersCommandHandler(conf, db))
		bot.AddCommandHandler(cmdStats, statsCommandHandler(conf, db))
		bot.AddCommandHandler(cmdHelp, helpCommandHandler(conf, db))
		bot.AddCommandHandler(cmdCancel, cancelCommandHandler(conf, db))
		bot.AddCommandHandler(cmdPrivacy, privacyCommandHandler(conf, db))
		bot.SetNoMatchingCommandHandler(noSuchCommandHandler(conf, db))

		// poll updates
		bot.StartPollingUpdates(0, intervalSeconds, func(b *tg.Bot, update tg.Update, err error) {
			if err == nil {
				if !isAllowed(conf, update) {
					logDebug(conf, "not allowed: %s", userNameFromUpdate(update))
					return
				}

				// type not supported
				if message := messageFromUpdate(update); message != nil {
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
func handleMessage(ctx context.Context, bot *tg.Bot, conf config, db *Database, gtc *gt.Client, update tg.Update, message tg.Message) {
	var msg string

	chatID := message.Chat.ID

	options := tg.OptionsSendMessage{}.
		SetReplyMarkup(defaultReplyMarkup())

	// 'is typing...'
	bot.SendChatAction(chatID, tg.ChatActionTyping, tg.OptionsSendChatAction{})

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
							what,
							datetimeToStr(when),
						)
					} else {
						msg = fmt.Sprintf(msgSaveFailedFormat, what, err)
					}
				} else if len(parsed) > 0 {
					if _, err := db.SaveTemporaryMessage(chatID, message.MessageID, parsed[0].Message); err == nil {
						msg = fmt.Sprintf(msgSelectWhat, parsed[0].Message)

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

	// fallback message
	if len(msg) <= 0 {
		msg = msgError
	}

	// send message
	if sent := bot.SendMessage(chatID, msg, options); !sent.Ok {
		logError(db, "failed to send message: %s", *sent.Description)
	}
}

// handle allowed callback query from telegram bot api
func handleCallbackQuery(b *tg.Bot, db *Database, query tg.CallbackQuery) {
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
	if apiResult := b.AnswerCallbackQuery(
		query.ID,
		tg.OptionsAnswerCallbackQuery{}.
			SetText(msg),
	); apiResult.Ok {
		// edit message and remove inline keyboards
		options := tg.OptionsEditMessageText{}.
			SetIDs(query.Message.Chat.ID, query.Message.MessageID)
		if apiResult := b.EditMessageText(msg, options); !apiResult.Ok {
			logError(db, "failed to edit message text: %s", *apiResult.Description)
		}
	} else {
		logError(db, "failed to answer callback query: %s", prettify(query))
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
		options.SetReplyParameters(tg.NewReplyParameters(*messageID))
	}
	if res := bot.SendMessage(chatID, message, options); !res.Ok {
		logError(db, "failed to send message: %s", *res.Description)
	}
}

// type for parsed items
type parsedItem struct {
	Message   string
	When      time.Time
	Generated bool // if this item was generated by the bot (due to vague request)
}

// function declarations for genai model
func fnDeclarations(conf config) []*genai.FunctionDeclaration {
	return []*genai.FunctionDeclaration{
		{
			Name:        fnNameInferDatetime,
			Description: fnDescriptionInferDatetime,
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					fnArgNameInferredDatetime: {
						Type:        genai.TypeString,
						Description: fmt.Sprintf(fnArgDescriptionInferredDatetime, conf.DefaultHour),
						Nullable:    false,
					},
					fnArgNameMessageToSend: {
						Type:        genai.TypeString,
						Description: fnArgDescriptionMessageToSend,
						Nullable:    false,
					},
				},
				Nullable: false,
			},
		},
	}
}

// handle function call
func handleFnCall(conf config, fn genai.FunctionCall) (result []parsedItem, err error) {
	logDebug(conf, "[verbose] handling function call: %s", prettify(fn))

	result = []parsedItem{}

	if fn.Name == fnNameInferDatetime {
		datetime := val[string](fn.Args, fnArgNameInferredDatetime)
		message := val[string](fn.Args, fnArgNameMessageToSend)

		if message != "" && datetime != "" {
			if t, e := time.ParseInLocation(datetimeFormat, datetime, _location); e == nil {
				result = append(result, parsedItem{
					Message:   message,
					When:      t,
					Generated: false,
				})
			} else {
				err = fmt.Errorf("failed to parse '%s' (%s) in function call: %s", fnArgNameInferredDatetime, e, prettify(fn.Args))
			}
		} else {
			err = fmt.Errorf("invalid `%s` and/or `%s` in function call: %s", fnArgNameInferredDatetime, fnArgNameMessageToSend, prettify(fn.Args))
		}
	} else {
		err = fmt.Errorf("no function declaration for name: %s", fn.Name)
	}

	if err != nil {
		logDebug(conf, "[verbose] there was an error with returned function call: %s", err)
	}

	return result, err
}

// get value for given `key` from `m`, return the zero value if it has no such key
func val[T any](m map[string]any, key string) T {
	var r T

	if v, exists := m[key]; exists {
		if v, ok := v.(T); ok {
			r = v
		}
	}

	return r
}

// prettify given value in indented JSON format (for debugging purporse)
func prettify(v any) string {
	if bytes, err := json.MarshalIndent(v, "", "  "); err == nil {
		return string(bytes)
	}

	return fmt.Sprintf("%+v", v)
}

// parse given string, generate items from the parsed ones, and return them
func parse(ctx context.Context, conf config, db *Database, gtc *gt.Client, message tg.Message, text string) (result []parsedItem, errs []error) {
	result = []parsedItem{}
	errs = []error{}

	chatID := message.Chat.ID
	userID := message.From.ID
	username := userName(message.From)

	// options for generation
	opts := &gt.GenerationOptions{
		// set function declarations
		Tools: []*genai.Tool{
			{
				FunctionDeclarations: fnDeclarations(conf),
			},
		},
	}
	// NOTE: `genai.FunctionCallingAny` is only available for `gemini-1.5-pro*`
	//
	// https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/function-calling
	if strings.Contains(conf.GoogleGenerativeModel, "gemini-1.5-pro") {
		opts.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				// FIXME: googleapi: Error 400: Function calling mode `ANY` is not enabled for api version v1beta
				/*
					Mode: genai.FunctionCallingAny,
					AllowedFunctionNames: []string{
						fnNameInferDatetime,
					},
				*/
				Mode: genai.FunctionCallingAuto,
			},
		}
	} else {
		opts.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingAuto,
			},
		}
	}

	// generate text
	var numTokensInput, numTokensOutput int32
	if generated, err := gtc.Generate(
		ctx,
		text,
		nil,
		opts,
	); err == nil {
		logDebug(conf, "[verbose] generated: %s", prettify(generated))

		// token counts
		numTokensInput, numTokensOutput = generated.UsageMetadata.PromptTokenCount, generated.UsageMetadata.CandidatesTokenCount

		if len(generated.Candidates) <= 0 {
			logError(db, "there was no returned candidate")
		} else {
			for _, candidate := range generated.Candidates {
				content := candidate.Content

				if len(content.Parts) > 0 {
					for _, part := range content.Parts {
						if fnCall, ok := part.(genai.FunctionCall); ok { // if it is a function call,
							if handled, err := handleFnCall(conf, fnCall); err == nil {
								// append result
								result = append(result, handled...)
							} else {
								errs = append(errs, err)

								logError(db, "failed to handle function call: %s", err)
							}
							break
						}
					}
				} else {
					errs = append(errs, fmt.Errorf("no part in content"))

					logError(db, "there was no part in the returned content")
				}
			}

			if len(result) <= 0 {
				errs = append(errs, fmt.Errorf("no function call in parts"))

				logError(db, "there was no usable function call in the returned parts")
			}
		}
	} else {
		errs = append(errs, fmt.Errorf("failed to generate text: %s", errorString(err)))

		// log failure
		savePromptAndResult(db, chatID, userID, username, text, int(numTokensInput), int(numTokensOutput), false)

		logError(db, "failed to generate text: %s", errorString(err))
	}

	// log success
	if len(errs) <= 0 {
		savePromptAndResult(db, chatID, userID, username, text, int(numTokensInput), int(numTokensOutput), true)
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

// save prompt and its result to logs database
func savePromptAndResult(db *Database, chatID, userID int64, username string, prompt string, promptTokens int, resultTokens int, resultSuccessful bool) {
	if db != nil {
		if err := db.SavePrompt(Prompt{
			ChatID:   chatID,
			UserID:   userID,
			Username: username,
			Text:     prompt,
			Tokens:   promptTokens,
			Result: ParsedItem{
				Successful: resultSuccessful,
				Tokens:     resultTokens,
			},
		}); err != nil {
			log.Printf("failed to save prompt & result to database: %s", err)
		}
	}
}

// generate a help message with version info
func helpMessage(conf config) string {
	return fmt.Sprintf(msgHelp, conf.GoogleGenerativeModel, version.Build(version.OS|version.Architecture|version.Revision), githubPageURL)
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
			send(b, conf, db, msg, chatID, nil)
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
			if sent := b.SendMessage(chatID, msg, options); !sent.Ok {
				logError(db, "failed to send message: %s", *sent.Description)
			}
		}
	}
}

// return a /privacy command handler
func privacyCommandHandler(conf config, db *Database) func(b *tg.Bot, update tg.Update, args string) {
	return func(b *tg.Bot, update tg.Update, args string) {
		if message := messageFromUpdate(update); message != nil {
			chatID := message.Chat.ID

			send(b, conf, db, msgPrivacy, chatID, nil)
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

			send(b, conf, db, helpMessage(conf), chatID, &messageID)
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

			send(b, conf, db, fmt.Sprintf(msgCmdNotSupported, cmd), chatID, &messageID)
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
	return tg.NewReplyKeyboardMarkup( // show keyboards
		[][]tg.KeyboardButton{
			tg.NewKeyboardButtons(cmdListReminders, cmdCancel, cmdStats),
			tg.NewKeyboardButtons(cmdPrivacy, cmdHelp),
		}).
		SetResizeKeyboard(true)
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
	buttons = append(buttons, []tg.InlineKeyboardButton{
		tg.NewInlineKeyboardButton(msgCancel).
			SetCallbackData(cmdCancel),
	})

	return buttons
}

// format given time to string
func datetimeToStr(t time.Time) string {
	return t.In(_location).Format(datetimeFormat)
}

// convert error to string
func errorString(err error) (error string) {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return fmt.Sprintf("googleapi error: %s", gerr.Body)
	} else {
		return err.Error()
	}
}
