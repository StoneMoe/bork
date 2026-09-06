package app

// GetSystemLanguage reads the user's UI language, rather than the locale used
// for dates and numbers. The frontend owns supported-language selection.
func (a *App) GetSystemLanguage() string {
	return systemLanguage()
}
