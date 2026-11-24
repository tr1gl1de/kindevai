package main

import (
	"context"
	"errors"

	identity "github.com/tr1gl1de/kindevai/internal/identity/domain"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// botTokenVault keeps the Telegram bot token in the integrations context's encrypted vault (slot
// telegram.bot_token) for the identity context's Mini App sign-in, translating its errors.
type botTokenVault struct{ vault *integrationsapp.Vault }

func (v botTokenVault) BotToken(ctx context.Context) (string, error) {
	s, err := v.vault.Credential(ctx, integrations.SecretTelegramBotToken)
	if errors.Is(err, integrations.ErrSecretNotSet) {
		return "", identity.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.Reveal(), nil
}

func (v botTokenVault) SetBotToken(ctx context.Context, token, by string) error {
	s, err := integrations.NewSecret(token)
	if err != nil {
		return err
	}
	return v.vault.Put(ctx, integrations.SecretTelegramBotToken, s, by)
}
