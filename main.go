package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/wechatpay-apiv3/wechatpay-go/core/auth/verifiers"
	"github.com/wechatpay-apiv3/wechatpay-go/core/notify"
	"github.com/wechatpay-apiv3/wechatpay-go/services/payments"
	"github.com/wechatpay-apiv3/wechatpay-go/utils"
)

func main() {
	config := koanf.New(".")
	if err := config.Load(file.Provider("config.yml"), yaml.Parser()); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// init wechat pay handler
	wechatPublicKey, err := utils.LoadPublicKey(config.MustString("wechat.publicKey"))
	if err != nil {
		log.Fatalf("failed to load wechat pay publicKey: %v", err)
	}
	wechatHandler, err := notify.NewRSANotifyHandler(
		config.MustString("wechat.apiV3Key"),
		verifiers.NewSHA256WithRSAPubkeyVerifier(config.MustString("wechat.publicKeyID"), *wechatPublicKey),
	)
	if err != nil {
		log.Fatalf("failed to create wechat pay notify handler: %v", err)
	}

	// init resty
	client := resty.New()
	client.SetTimeout(4 * time.Second) // https://pay.weixin.qq.com/doc/v3/merchant/4012791882 需要在5秒内完成处理
	client.SetRetryCount(5)
	forwards := config.Strings("forwards")

	// init fiber app
	app := fiber.New(fiber.Config{
		AppName: "wechatpay-notify-gateway",
	})
	app.Post("/notify", func(c fiber.Ctx) error {
		req, err := adaptor.ConvertRequest(c, true)
		if err != nil {
			return c.Res().Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"code":    "FAIL",
				"message": err.Error(),
			})
		}

		transaction := new(payments.Transaction)
		if _, err = wechatHandler.ParseNotifyRequest(context.TODO(), req, transaction); err != nil {
			return c.Res().Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"code":    "FAIL",
				"message": err.Error(),
			})
		}

		// 转发请求
		if _, err = url.ParseRequestURI(*transaction.Attach); err == nil {
			if err := forwardRequest(client, c, *transaction.Attach); err != nil {
				return err
			}
		} else {
			for _, forward := range forwards {
				if err := forwardRequest(client, c, forward); err != nil {
					return err
				}
			}
		}

		return c.SendStatus(fiber.StatusNoContent)
	})

	if err = app.Listen(config.MustString("address"), fiber.ListenConfig{
		ListenerNetwork:       fiber.NetworkTCP,
		EnablePrintRoutes:     config.Bool("debug"),
		DisableStartupMessage: !config.Bool("debug"),
	}); err != nil {
		log.Fatal(fmt.Errorf("failed to start server: %w", err))
	}
}

func forwardRequest(client *resty.Client, c fiber.Ctx, targetURL string) error {
	request := client.R().SetBody(c.Body())
	request.Header = c.GetReqHeaders()

	resp, err := request.Post(targetURL)
	if err != nil {
		return c.Res().Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"code":    "FAIL",
			"message": fmt.Sprintf("failed to forward request: %v", err),
		})
	}

	if resp.StatusCode() >= 400 {
		return c.Res().Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"code":    "FAIL",
			"message": fmt.Sprintf("target server responded with status %d: %s", resp.StatusCode(), resp.String()),
		})
	}

	return nil
}
