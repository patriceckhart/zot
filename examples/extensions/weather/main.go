// weather — demonstrates a tool-providing extension.
//
// Registers ONE tool the LLM can call:
//
//	weather(city: string) -> short fake weather string
//
// Build it:
//
//	cd examples/extensions/weather
//	go build -o weather .
//
// Then drop it next to its extension.json under
// $ZOT_HOME/extensions/weather/, or run `zot ext install ./weather`
// from this directory. After that, asking zot anything like
// "what's the weather in Berlin?" makes the model call the tool.
package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/pkg/zotext"
)

const weatherSchema = `{
  "type": "object",
  "properties": {
    "city": {
      "type": "string",
      "description": "City name to look up the weather for."
    }
  },
  "required": ["city"]
}`

func main() {
	ext := zotext.New("weather", "1.0.0")

	ext.Tool("weather", "Get the current weather for a city.", json.RawMessage(weatherSchema), func(args json.RawMessage) zotext.ToolResult {
		var in struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return zotext.TextErrorResult("invalid args: " + err.Error())
		}
		city := strings.TrimSpace(in.City)
		if city == "" {
			return zotext.TextErrorResult("city is required")
		}
		// Stable fake "weather": derive temperature + condition from a
		// hash of the city name so the same city always reports the
		// same thing. No API calls, no timezone math, no surprises.
		return zotext.TextResult(fakeWeather(city))
	})

	if err := ext.Run(); err != nil {
		ext.Logf("fatal: %v", err)
	}
}

func fakeWeather(city string) string {
	sum := sha1.Sum([]byte(strings.ToLower(city)))
	temp := (int(sum[0])%30 - 5) // -5..24 °C
	conditions := []string{"sunny", "cloudy", "drizzle", "rain", "thunderstorm", "fog", "snow", "windy"}
	cond := conditions[int(sum[1])%len(conditions)]
	return fmt.Sprintf("%s: %d°C, %s (this is a deterministic fake; the weather extension is a demo)", city, temp, cond)
}
