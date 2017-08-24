/*
 * This file is part of arduino-cli.
 *
 * arduino-cli is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program; if not, write to the Free Software
 * Foundation, Inc., 51 Franklin St, Fifth Floor, Boston, MA  02110-1301  USA
 *
 * As a special exception, you may use this file as part of a free software
 * library without restriction.  Specifically, if other files instantiate
 * templates or use macros or inline functions from this file, or you compile
 * this file and link it with other files to produce an executable, this
 * file does not by itself cause the resulting executable to be covered by
 * the GNU General Public License.  This exception does not however
 * invalidate any other reasons why the executable file might be covered by
 * the GNU General Public License.
 *
 * Copyright 2017 BCMI LABS SA (http://www.arduino.cc/)
 */

/*
Package auth uses the `oauth2 authorization_code` flow to authenticate with Arduino

If you have the username and password of a user, you can just instantiate a client with sane defaults:

  client := auth.New()

and then call the Token method to obtain a Token object with an AccessToken and a RefreshToken

  token, err := client.Token(username, password)

If instead you already have a token but want to refresh it, just call

  token, err := client.refresh(refreshToken)
*/
package auth

import (
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// New returns an auth configuration with sane defaults
func New() *Config {
	return &Config{
		CodeURL:     "https://hydra.arduino.cc/oauth2/auth",
		TokenURL:    "https://hydra.arduino.cc/oauth2/token",
		ClientID:    "cli",
		RedirectURI: "http://auth.arduino.cc:5000",
		Scopes:      "profile:core offline",
	}
}

// Token authenticates with the given username and password and returns a Token object
func (c *Config) Token(user, pass string) (*Token, error) {
	// We want to make sure we send the proper cookies each step, so we don't follow redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Request authentication page
	url, cookies, err := c.requestAuth(client)
	if err != nil {
		return nil, errors.Wrap(err, "get the auth page")
	}

	// Authenticate
	code, err := c.authenticate(client, cookies, url, user, pass)
	if err != nil {
		return nil, errors.Wrap(err, "authenticate")
	}

	// Request token
	token, err := c.requestToken(client, code)
	if err != nil {
		return nil, errors.Wrap(err, "request token")
	}
	return token, nil
}

// Refresh exchanges a token for a new one
func (c *Config) Refresh(token string) (*Token, error) {
	client := http.Client{}
	query := url.Values{}
	query.Add("refresh_token", token)
	query.Add("client_id", c.ClientID)
	query.Add("redirect_uri", c.RedirectURI)
	query.Add("grant_type", "refresh_token")

	req, err := http.NewRequest("POST", c.TokenURL, strings.NewReader(query.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cli", "")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	data := Token{}

	err = json.Unmarshal(body, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

type User struct{}

// LoggedUser returns the logged User's data.
func (t *Token) LoggedUser() (*User, error) {
	req, err := http.NewRequest("GET", "https://auth.arduino.cc/v1/users/byID/me", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("cli", "")
	req.Header.Add("Authorization", "Bearer "+t.Access)
	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "LoggedUser")
	}
	if resp.StatusCode != 200 {
		return nil, errors.New(resp.Status)
	}
	defer resp.Body.Close()
	content, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "LoggedUser")
	}
	var ret User
	err = json.Unmarshal(content, &ret)
	if err != nil {
		return nil, errors.Wrap(err, "LoggedUser")
	}
	return &ret, nil
}

// cookies keeps track of the cookies for each request
type cookies map[string][]*http.Cookie

// requestAuth calls hydra and follows the redirects until it reaches the authentication page. It saves the cookie it finds so it can apply them to subsequent requests
func (c *Config) requestAuth(client *http.Client) (string, cookies, error) {
	uri, err := url.Parse(c.CodeURL)
	if err != nil {
		return "", nil, err
	}

	query := uri.Query()
	query.Add("client_id", c.ClientID)
	query.Add("state", randomString(8))
	query.Add("scope", c.Scopes)
	query.Add("response_type", "code")
	query.Add("redirect_uri", c.RedirectURI)
	uri.RawQuery = query.Encode()

	// Navigate to hydra request page
	res, err := client.Get(uri.String())
	if err != nil {
		return "", nil, err
	}

	cookies := cookies{}
	cookies["hydra"] = res.Cookies()

	// Navigate to auth request page
	res, err = client.Get(res.Header.Get("Location"))
	if err != nil {
		return "", nil, err
	}

	cookies["auth"] = res.Cookies()
	return res.Request.URL.String(), cookies, err
}

// authenticate uses the user and pass to pass the authentication challenge and returns the authorization_code
func (c *Config) authenticate(client *http.Client, cookies cookies, uri, user, pass string) (string, error) {
	// Find csrf
	csrf := ""
	for _, cookie := range cookies["auth"] {
		if cookie.Name == "_csrf" {
			csrf = cookie.Value
		}
	}
	query := url.Values{}
	query.Add("username", user)
	query.Add("password", pass)
	query.Add("csrf", csrf)

	req, err := http.NewRequest("POST", uri, strings.NewReader(query.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	// Apply cookies
	for _, cookie := range cookies["auth"] {
		req.AddCookie(cookie)
	}

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if res.StatusCode != 302 {
		return "", errors.New("authentication failed")
	}

	// Follow redirect to hydra
	req, err = http.NewRequest("GET", res.Header.Get("Location"), nil)
	if err != nil {
		return "", err
	}

	for _, cookie := range cookies["hydra"] {
		req.AddCookie(cookie)
	}

	res, err = client.Do(req)
	if err != nil {
		return "", err
	}

	redir, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		return "", err
	}

	return redir.Query().Get("code"), nil
}

func (c *Config) requestToken(client *http.Client, code string) (*Token, error) {
	query := url.Values{}
	query.Add("code", code)
	query.Add("client_id", c.ClientID)
	query.Add("redirect_uri", c.RedirectURI)
	query.Add("grant_type", "authorization_code")

	req, err := http.NewRequest("POST", c.TokenURL, strings.NewReader(query.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cli", "")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	data := Token{}

	err = json.Unmarshal(body, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

// randomString generates a string of random characters of fixed length.
// stolen shamelessly from https://stackoverflow.com/questions/22892120/how-to-generate-a-random-string-of-a-fixed-length-in-golang
const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

var src = rand.NewSource(time.Now().UnixNano())

func randomString(n int) string {
	b := make([]byte, n)
	// A src.Int63() generates 63 random bits, enough for letterIdxMax characters!
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return string(b)
}
