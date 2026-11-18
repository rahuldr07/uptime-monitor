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

// Subjects of the 2FA notices, shared with the tests.
const (
	msgMFALimitedSubject   = "Too many two-factor attempts"
	msgRecoveryUsedSubject = "A recovery code was used on your account"
	msgMFAOnSubject        = "Two-factor authentication was turned on"
	msgMFAOffSubject       = "Two-factor authentication was turned off"
	msgMFAResetSubject     = "Two-factor authentication was turned off by an administrator"
	msgRecoveryNewSubject  = "Your recovery codes were replaced"
)

// stamp ends a notice with when it happened, in the owner's zone.
func stamp(at time.Time, loc *time.Location) string { return "\nWhen: " + notify.FormatTime(at, loc) + "\n" }

// mfaLimitedMail warns the owner that codes were guessed. method is how the
// guesser got as far as the code: "password", "google" or "github"; "" is
// someone already signed in.
func mfaLimitedMail(base, method string, at time.Time, loc *time.Location) (subject, body string) {
	var text string
	switch method {
	case "google", "github":
		p := providerLabel(method)
		text = fmt.Sprintf("Someone signed in with your %s account but did not enter your two-factor code correctly. If it wasn't you, secure your %s account, then disconnect it under Sign-in methods.\n\n%s/settings/security\n", p, p, base)
	case "":
		text = fmt.Sprintf("Someone signed in to your account entered a wrong two-factor code too many times. If it wasn't you, sign out everywhere and change your password.\n\n%s/settings/security\n%s/forgot\n", base, base)
	default:
		text = fmt.Sprintf("Someone entered your password but not your two-factor code. If it wasn't you, change your password.\n\n%s/forgot\n", base)
	}
	return msgMFALimitedSubject, text + stamp(at, loc)
}

func recoveryUsedMail(base string, left int, signIn bool, at time.Time, loc *time.Location) (subject, body string) {
	what := "A recovery code was used on your account."
	if signIn {
		what = "A recovery code was used to sign in."
	}
	return msgRecoveryUsedSubject,
		fmt.Sprintf("%s %d left.\n\nIf this wasn't you, change your password, then make new recovery codes again under Security in your Settings:\n\n%s/forgot\n%s/settings/security\n", what, left, base, base) + stamp(at, loc)
}

func mfaOnMail(base string, at time.Time, loc *time.Location) (subject, body string) {
	return msgMFAOnSubject,
		fmt.Sprintf("Two-factor authentication was turned on. Your other devices were signed out.\n\nIf this wasn't you, reset your password and ask this server's administrator to reset two-factor authentication.\n\n%s/forgot\n", base) + stamp(at, loc)
}

func mfaOffMail(base string, at time.Time, loc *time.Location) (subject, body string) {
	return msgMFAOffSubject,
		fmt.Sprintf("Two-factor authentication was turned off. Your other devices were signed out.\n\nIf this wasn't you, reset your password and turn it on again:\n\n%s/forgot\n", base) + stamp(at, loc)
}

func mfaResetMail(base string, at time.Time, loc *time.Location) (subject, body string) {
	return msgMFAResetSubject,
		fmt.Sprintf("An administrator turned off two-factor authentication on your account. Sign in, then turn it on again under Security in your Settings. If you didn't ask for this, contact the administrator.\n\n%s/login\n", base) + stamp(at, loc)
}

func recoveryNewMail(base string, at time.Time, loc *time.Location) (subject, body string) {
	return msgRecoveryNewSubject,
		fmt.Sprintf("New recovery codes were created and the old ones stopped working. Your other devices were signed out.\n\nIf this wasn't you, reset your password, sign in, then make new recovery codes again:\n\n%s/forgot\n", base) + stamp(at, loc)
}
