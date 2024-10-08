package listeners

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Zaydo123/token-processor/internal/config"
	"github.com/Zaydo123/token-processor/internal/redis/client"
	ConsumerEvents "github.com/Zaydo123/token-processor/internal/redis/models"
	"github.com/Zaydo123/token-processor/internal/redis/tasks/get"
	"github.com/Zaydo123/token-processor/internal/token/models"
	parser "github.com/Zaydo123/token-processor/internal/token/parser"
	"github.com/Zaydo123/token-processor/internal/token/prices"
	"github.com/Zaydo123/token-processor/internal/token/swaps"
	topownership "github.com/Zaydo123/token-processor/internal/token/top-ownership"
	"github.com/davecgh/go-spew/spew"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

var rdb *redis.Client // Redis client

func receiveBurnMessages(ctx context.Context, wg *sync.WaitGroup) {
	// Receive messages from the burns channel
	defer wg.Done()
	pubsub := rdb.Subscribe(ctx, config.ApplicationConfig.BurnsChannel)

	// Wait for messages
	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			log.Error().Err(err).Msg("Error receiving message")
			return
		}

		// Unmarshal the entire message directly into a BurnEvent
		var burnEvent ConsumerEvents.BurnEvent
		err = json.Unmarshal([]byte(msg.Payload), &burnEvent)
		if err != nil {
			log.Error().Err(err).Msg("Error parsing burn event")
			continue
		}

		// Log the fully populated BurnEvent, which includes the populated Data field
		log.Info().Msgf("Parsed BurnEvent: %+v", burnEvent)
		// TODO: Process the burn event further
	}
}

func receiveNewPairsMessages(ctx context.Context, wg *sync.WaitGroup, tokenMap *map[string]*models.Token) {
	// Receive messages from the new pairs channel
	defer wg.Done()
	pubsub := rdb.Subscribe(ctx, config.ApplicationConfig.NewPairsChannel)

	// Wait for messages
	for {

		msg, errReceive := pubsub.ReceiveMessage(ctx)
		if errReceive != nil {
			log.Error().Err(errReceive).Msg("Error receiving message")
			return
		}

		// Parse the message into a NewPairEvent
		var newPairEvent ConsumerEvents.NewPairEvent
		errUnmarshal := json.Unmarshal([]byte(msg.Payload), &newPairEvent)

		if errUnmarshal != nil {
			log.Error().Err(errUnmarshal).Msg("Error parsing new pair event")
			continue
		}

		log.Info().Msgf("Parsed NewPairEvent: %+v", newPairEvent)

		// STEP 1 : Get All Token Info and Parse

		tp := parser.NewTokenParser()
		//time the function
		start := time.Now()

		//basepoolaccount string to solana.PublicKey
		basePoolAccount := solana.MustPublicKeyFromBase58(newPairEvent.Data.BasePoolAccount)
		quotePoolAccount := solana.MustPublicKeyFromBase58(newPairEvent.Data.QuotePoolAccount)

		fetchContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		tokenObj, errRunAll := tp.RunAll(fetchContext, newPairEvent.Data.BaseToken, rpc.CommitmentFinalized, &basePoolAccount, &quotePoolAccount)
		tokenObj.IPO = newPairEvent.Data.BlockTime // seconds not milliseconds

		end := time.Now()

		if errRunAll != nil {
			log.Error().Err(errRunAll).Msg("Failed to get token info")
			return
		}

		elapsed := end.Sub(start)

		spew.Dump(tokenObj)

		log.Info().Msgf("Time taken to get all info: %s", elapsed)

		// STEP 2 : Add to Token Map
		(*tokenMap)[newPairEvent.Data.BaseToken] = tokenObj

		// STEP 3 : Start Price Service
		deadlinePrice := time.Now().Add(time.Duration(config.ApplicationConfig.PriceFollowTime) * time.Second * 5) // 5 times the follow time as deadline (in case of any issues)
		contextPrice, cancelPrice := context.WithDeadline(ctx, deadlinePrice)
		defer cancelPrice()
		go prices.FollowPrice(contextPrice, tp, tokenObj, config.ApplicationConfig.PriceFollowTime, config.ApplicationConfig.PriceInterval)

		// STEP 4 : Start Top Ownership Service
		deadlineOwner := time.Now().Add(time.Duration(config.ApplicationConfig.OwnersFollowTime) * time.Second * 5) // 5 times the follow time as deadline (in case of any issues)
		contextOwner, cancel := context.WithDeadline(ctx, deadlineOwner)
		defer cancel()
		go topownership.FollowTopOwnership(contextOwner, tp, tokenObj, config.ApplicationConfig.OwnersFollowTime, config.ApplicationConfig.OwnersInterval)

		// Done

	}
}

func receiveSwapMessages(ctx context.Context, wg *sync.WaitGroup, tokenMap *map[string]*models.Token) {
	// Receive messages from the swaps channel
	defer wg.Done()
	pubsub := rdb.Subscribe(ctx, config.ApplicationConfig.SwapsChannel)

	// Wait for messages
	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			log.Error().Err(err).Msg("Error receiving message")
			return
		}

		//log.Info().Msgf("Received message: %s", msg.Payload)

		// Unmarshal the entire message directly into a SwapEvent
		var swapEvent ConsumerEvents.SwapEvent
		err = json.Unmarshal([]byte(msg.Payload), &swapEvent)
		if err != nil {
			log.Error().Err(err).Msg("Error parsing swap event")
			continue
		}

		// log.Info().Msgf("Parsed SwapEvent: %+v", swapEvent)

		//find token in token map and process swap event for that token
		token, ok := (*tokenMap)[swapEvent.Data.TokenAddress]
		if !ok {
			log.Error().Msg("Token not found in token map... searching redis for cached state")
			foundToken, notFoundError := get.GetTokenData(swapEvent.Data.TokenAddress)
			if notFoundError != nil {
				log.Error().Msg("Token not found in redis cache")
				continue
			}
			//add to token map
			(*tokenMap)[swapEvent.Data.TokenAddress] = foundToken
			log.Info().Msgf("Revived token from cache: %s", swapEvent.Data.TokenAddress)
			token = foundToken
		}
		swaps.ProcessSwapEvent(token, swapEvent)
	}
}

func StartServices(ctx context.Context, wg *sync.WaitGroup, tokenMap *map[string]*models.Token) {

	// Initialize the Redis client
	client.InitRedisClient(config.ApplicationConfig.RedisHost, config.ApplicationConfig.RedisPort, config.ApplicationConfig.RedisPassword, &ctx)
	rdb = client.GetRedisClient()

	// Start all the services
	log.Info().Msg("Starting services")

	// Receive burns messages
	wg.Add(1)
	go receiveBurnMessages(ctx, wg)

	// Receive new pairs messages
	wg.Add(1)
	go receiveNewPairsMessages(ctx, wg, tokenMap)

	// Receive swaps messages
	wg.Add(1)
	go receiveSwapMessages(ctx, wg, tokenMap)

	log.Info().Msg("All services started")

	// Wait for all services to finish
	if wg != nil {
		wg.Wait()
	} else {
		log.Warn().Msg("Wait group is nil")
	}

	log.Info().Msg("All services stopped")
}
