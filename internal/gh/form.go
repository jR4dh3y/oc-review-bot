package gh

import "net/url"

func encodeForm(kv map[string]string) string {
	vals := url.Values{}
	for k, v := range kv {
		vals.Set(k, v)
	}
	return vals.Encode()
}
