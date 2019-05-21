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
var slackTipAmount="100" string
var tokenAddress="0x0BA7846EfbDa22e8dE9C6d225EDE295510CEdb4E" string
var ethApiEndpoint="https://ropsten.infura.io/a1e55b9084034d9181177a75e1badfea" string
var ethKeyJson="{address:8d0cb63a00f8130faa634986f16981e7fd9cde2b,Crypto:{ciphertext:ce1b209d1a7a248b9f7acb5070c2b467783394312366df609f87833b509bdfff,cipherparams:{iv:612e29c76777145f7336a3160cc53dfb},cipher:aes-128-ctr,kdf:scrypt,kdfparams:{dklen:32,salt:de036f90fe2632308e4d4b944b25f570960830189850659c68efb00f1deb8267,n:8192,r:8,p:1},mac:fdbcb7099f69217a9696560457ae1bf466269720da8df55a983ab4028d643f67},id:025135e4-6def-4b58-8b62-e8b730b4c9ba,version:3}" string
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
