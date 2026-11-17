package web

import (
	"fmt"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/notify"
)

// Email copy lives here and nowhere else. No message carries user-supplied
// text; links are built from cfg.BaseURL.

func verifyMail(link string) (subject, body string) {
	return "Confirm your email",
		fmt.Sprintf("Open this link to finish signing up. It is valid for 24 hours.\n\n%s\n\nIf you did not sign up, ignore this email.\n", link)
}

func duplicateMail(base string) (subject, body string) {
	return "Sign-up attempt with your address",
		fmt.Sprintf("Someone tried to sign up with your email address. If it was you, sign in or reset your password:\n\n%s/login\n%s/forgot\n", base, base)
}

func resetMail(link string) (subject, body string) {
	return "Reset your password",
		fmt.Sprintf("Open this link to choose a new password. It is valid for 1 hour.\n\n%s\n\nIf you did not ask for this, ignore this email.\n", link)
}

func setPasswordMail(link string) (subject, body string) {
	return "Set a password for your Uptime account",
		fmt.Sprintf("Open this link to choose a password for your account. It is valid for 1 hour.\n\n%s\n\nIf you did not ask for this, ignore this email.\n", link)
}

func passwordChangedMail(base string) (subject, body string) {
	return "Your password was changed",
		fmt.Sprintf("The password for your account was just changed. If this was not you, reset it now:\n\n%s/forgot\n", base)
}

func signInMethodMail(base, provider string, added bool, at time.Time, loc *time.Location) (subject, body string) {
	verb, subject := "removed from", "A sign-in method was removed from your account"
	if added {
		verb, subject = "added to", "A sign-in method was added to your account"
	}
	return subject,
		fmt.Sprintf("The %s sign-in method was %s your account on %s.\n\nIf this was not you, disconnect it under Sign-in methods in your Settings, then reset your password and sign out everywhere:\n\n%s/forgot\n%s/settings/security\n",
			provider, verb, notify.FormatTime(at, loc), base, base)
}
