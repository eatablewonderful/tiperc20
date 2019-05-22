package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/nlopes/slack"

	_ "github.com/lib/pq"
)

var slackBotId string
var slackBotToken string
var slackTipReaction="tip" string
var slackTipAmount="100000" string
var tokenAddress="0x0BA7846EfbDa22e8dE9C6d225EDE295510CEdb4E" string
var ethApiEndpoint="https://ropsten.infura.io/v3/a1e55b9084034d9181177a75e1badfea" string
var ethKeyJson="{
	"version": 3,
	"id": "a7d7728d-be90-425a-a929-7d6fa92cef01",
	"address": "69c73153269242bea1e06a51577686ebdfff6dd2",
	"crypto": {
		"ciphertext": "c7e02f2d9efe111ddcd174264ae1f7ff042a5ce638212291f940b0fb77c3f948",
		"cipherparams": {
			"iv": "c1c6004d28c66f7c913bd8679a8395e7"
		},
		"cipher": "aes-128-ctr",
		"kdf": "scrypt",
		"kdfparams": {
			"dklen": 32,
			"salt": "f2b09dd74a7ec33c2dfb4fb5538ecbac3be3abfe7c815a928e157decf5f76ef9",
			"n": 8192,
			"r": 8,
			"p": 1
		},
		"mac": "0eef98dcd9a72df7eb86d26c0f42f3ce275c9edc2090aaa5ac791953c19157d4"
	}
}" string
var ethPassword="qwerty500" string

var httpdPort int

var cmdRegex = regexp.MustCompile("^<@[^>]+> ([^<]+) (?:<@)?([^ <>]+)(?:>)?")

func init() {
	slackBotToken = os.Getenv("SLACK_BOT_TOKEN")
	slackTipReaction = os.Getenv("SLACK_TIP_REACTION")
	slackTipAmount = os.Getenv("SLACK_TIP_AMOUNT")
	tokenAddress = os.Getenv("ERC20_TOKEN_ADDRESS")
	ethApiEndpoint = os.Getenv("ETH_API_ENDPOINT")
	ethKeyJson = os.Getenv("ETH_KEY_JSON")
	ethPassword = os.Getenv("ETH_PASSWORD")

	flag.IntVar(&httpdPort, "port", 20020, "port number")
}

func main() {
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tip20: https://github.com/eatablewonderful/tiperc20")
	})
	go func() {
		log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", httpdPort), nil))
	}()

	api := slack.New(slackBotToken)
	rtm := api.NewRTM()
	go rtm.ManageConnection()

Loop:
	for {
		select {
		case msg := <-rtm.IncomingEvents:
			switch ev := msg.Data.(type) {
			case *slack.ConnectedEvent:
				slackBotId = ev.Info.User.ID
			case *slack.MessageEvent:
				handleMessage(api, ev)
			case *slack.RTMError:
				fmt.Printf("Error: %s\n", ev.Error())
			case *slack.InvalidAuthEvent:
				fmt.Printf("Invalid credentials")
				break Loop
			case *slack.ReactionAddedEvent:
				handleReaction(api, ev)
			default:
				// Ignore unknown errors because it's emitted too much time
			}
		}
	}
}

func handleMessage(api *slack.Client, ev *slack.MessageEvent) {
	if !strings.HasPrefix(ev.Text, "<@"+slackBotId+">") {
		return
	}

	matched := cmdRegex.FindStringSubmatch(ev.Text)
	if len(matched) == 0 {
		fmt.Printf("Unknown command")
		return
	}
	fmt.Println(matched)
	switch matched[1] {
	case "tip":
		handleTipCommand(api, ev, matched[2])
	case "register":
		handleRegister(api, ev, matched[2])
	default:
		fmt.Printf("Unknown command")
	}
}

func handleReaction(api *slack.Client, ev *slack.ReactionAddedEvent) {
	if ev.Reaction != slackTipReaction {
		return
	}

	address := retrieveAddressFor(ev.ItemUser)
	if address == "" {
		sendSlackMessage(api, ev.ItemUser, `
:question: Please register your Ethereum address:

> @tiperc20 register YOUR_ADDRESS
		`)
	} else {
		tx, err := sendTokenTo(address)
		if err == nil {
			user, _ := api.GetUserInfo(ev.User)
			message := fmt.Sprintf(":+1: You got a token from @%s at %x", user.Profile.RealName, tx.Hash())
			sendSlackMessage(api, ev.ItemUser, message)
		}
	}
}

func handleTipCommand(api *slack.Client, ev *slack.MessageEvent, userID string) {
	address := retrieveAddressFor(userID)

	if address == "" {
		sendSlackMessage(api, userID, `
:question: Please register your Ethereum address:

> @tiperc20 register YOUR_ADDRESS
		`)
	} else {
		tx, err := sendTokenTo(address)
		if err != nil {
			sendSlackMessage(api, ev.Channel, ":x: "+err.Error())
		} else {
			user, _ := api.GetUserInfo(ev.User)
			message := fmt.Sprintf(":+1: You got a token from @%s at %x", user.Profile.RealName, tx.Hash())
			sendSlackMessage(api, userID, message)
		}
	}
}

func handleRegister(api *slack.Client, ev *slack.MessageEvent, address string) {
	userId := ev.User

	db, _ := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO accounts(slack_user_id, ethereum_address) VALUES ($1, $2)
		ON CONFLICT ON CONSTRAINT accounts_slack_user_id_key
		DO UPDATE SET ethereum_address=$2;
	`, userId, address)

	if err != nil {
		sendSlackMessage(api, ev.Channel, ":x: "+err.Error())
	} else {
		sendSlackMessage(api, ev.Channel, ":o: Registered `"+address+"`")
	}
}

func sendTokenTo(address string) (tx *types.Transaction, err error) {
	conn, err := ethclient.Dial(ethApiEndpoint)
	if err != nil {
		log.Printf("Failed to instantiate a Token contract: %v", err)
		return
	}

	token, err := NewToken(common.HexToAddress(tokenAddress), conn)
	if err != nil {
		log.Printf("Failed to instantiate a Token contract: %v", err)
		return
	}

	auth, err := bind.NewTransactor(strings.NewReader(ethKeyJson), ethPassword)
	if err != nil {
		log.Printf("Failed to create authorized transactor: %v", err)
		return
	}

	amount, err := strconv.ParseInt(slackTipAmount, 10, 64)
	if err != nil {
		log.Printf("Invalid tip amount: %v", err)
		return
	}

	tx, err = token.Transfer(auth, common.HexToAddress(address), big.NewInt(amount))
	if err != nil {
		log.Printf("Failed to request token transfer: %v", err)
		return
	}

	log.Printf("Transfer pending: 0x%x\n", tx.Hash())
	return
}

func sendSlackMessage(api *slack.Client, channel, message string) {
	_, _, err := api.PostMessage(channel, message, slack.PostMessageParameters{})
	if err != nil {
		log.Println(err)
	}
}

func retrieveAddressFor(userID string) (address string) {
	db, _ := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	defer db.Close()

	db.QueryRow(`
		SELECT ethereum_address FROM accounts WHERE slack_user_id = $1 LIMIT 1;
	`, userID).Scan(&address)

	return
}
