package host

import "regexp"

// The bot-challenge machinery: Cloudflare (and the hosts leaning on it)
// grades every visitor by reputation and challenges the ones it does not
// trust. A challenge is served as plain HTML at any status, names itself
// in its title, and decides with JavaScript in the browser — packrat,
// driving the protocol without a browser, can never pass one. Naming the
// refusal matters: it must not read as a rate limit (waiting changes
// nothing) or a layout change (the host's markup may be perfectly fine).

// challengeRes are the fingerprints of the refusal pages, anchored to
// their <title> heads so ordinary page copy never misreads as a
// challenge. The captched POST verdict rides in the body instead of a
// title, so it matches on the phrase.
var challengeRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)<title[^>]*>\s*just a moment`),
	regexp.MustCompile(`(?i)<title[^>]*>\s*attention required`),
	regexp.MustCompile(`(?i)captcha verification failed`),
}

// ClassifyChallenge reports a bot-challenge refusal in a failed exchange,
// or nil when nothing in it looks like one. The body is what identifies
// the refusal, whatever status it rides on.
func ClassifyChallenge(body []byte) error {
	for _, re := range challengeRes {
		if re.Match(body) {
			return ErrChallengeRequired
		}
	}
	return nil
}
