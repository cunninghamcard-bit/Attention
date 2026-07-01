# Run the headless kernel + the TUI viewer.
# The kernel resolves everything from ~/.along/agent config:
#   - model from settings.json ("defaultModel")
#   - key   from models.json ("apiKey"), or a provider env var (e.g. DEEPSEEK_API_KEY)
# With both configured, `make run` needs no flags and no env.
BIN := $(CURDIR)/bin

.PHONY: kernel tui run clean
kernel:
	mkdir -p $(BIN)
	go build -o $(BIN)/along ./cmd/along
tui: kernel
	cd cmd/tui && go run . --along-path $(BIN)/along
run: tui
clean:
	rm -rf $(BIN)
