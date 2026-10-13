package telegram

func localized(language, ru, en string) string {
	if language == "en" {
		return en
	}
	return ru
}

func (b *Bot) text(ru, en string) string {
	return localized(b.currentLanguage(), ru, en)
}
