// Written with assistance from Claude Sonnet 4.6
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// defining http timeout; not very useful here (local), very much so in prod
// const httpCallTimeout = 5 * time.Second

// URL and the client
type HTTPExchange struct {
	baseURL string
	client  *http.Client    

	// You could just call http.Get(url) or http.Post(url, ...) directly without it, but those use a shared default client with no timeout configured. By creating your own http.Client{} you get control over things like timeouts, redirects, and connection pooling.
	// just hygiene in case we want more control
}

// initialise the struct
func NewHTTPExchange(baseURL string) *HTTPExchange {
	h := &HTTPExchange{}
	h.baseURL = baseURL
	h.client = &http.Client{}
	return h
}

// this class needs to implement placeOrder, getOrder and cancelOrder

// placeorder
func (h *HTTPExchange) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*Order, error) {
	// req -> json format
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling place order request: %w", err)
	}

	// json -> bytes using bytes.NewReader(b)
	// build the request object
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/orders", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("creating place order request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// fire over the network
	resp, err := h.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("posting order: %w", err)
	}
	// resp.Body is streamed, closing it means we wont leak memory
	defer resp.Body.Close() 

	// if error; print the message returned by the mock server
	if resp.StatusCode != http.StatusOK {
		var errResp struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("exchange returned %d: %s", resp.StatusCode, errResp.Error)
	}

	// we're here : no error

	// exchange only returns {id, status: "accepted"} — not the full order
	var result struct{ ID string `json:"id"` }
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding place order response: %w", err)
	}

	o := &Order{}
	o.ID = result.ID
	o.Status = orderStatusOpen
	return o, nil
}


// getorder
func (h *HTTPExchange) GetOrder(ctx context.Context, orderID string) (*Order, error) {
	// no need to format input here
	// build the request object

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/orders/"+orderID, nil)
	if err != nil {
		return nil, fmt.Errorf("creating get order request: %w", err)
	}

	// fire overthe network
	resp, err := h.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("getting order %s: %w", orderID, err)
	}
	// defer becasue body is streamed as and when read
	defer resp.Body.Close()

	// if error; print the message returned by the mock server
	if resp.StatusCode != http.StatusOK {
		var errResp struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf("exchange returned %d for order %s: %s", resp.StatusCode, orderID, errResp.Error)
	}

	o := &Order{}
	// response -> order
	if err := json.NewDecoder(resp.Body).Decode(o); err != nil {
		return nil, fmt.Errorf("decoding get order response: %w", err)
	}
	return o, nil
}

// delete order

func (h *HTTPExchange) CancelOrder(ctx context.Context, orderID string) error {
	// build the req object
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.baseURL+"/orders/"+orderID, nil)
	if err != nil {
		return fmt.Errorf("creating cancel order request: %w", err)
	}

	// fire
	resp, err := h.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("cancelling order %s: %w", orderID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("exchange returned %d cancelling order %s: %s", resp.StatusCode, orderID, errResp.Error)
	}

	return nil
}


