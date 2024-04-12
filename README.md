# PoC for CVE-2023-45288

This is a proof-of-concept code for the CONTINUATION flood vulnerability found and documented by Bartek Nowotarski. The technical details are very well documented in his blog post [here](https://nowotarski.info/http2-continuation-flood-technical-details/).

This code borrows some inspiration from:

1. The PoC code for the rapid reset vulnerability from https://github.com/secengjeff/rapidresetclient
2. Test code added after the vulnerability was patched by the Go team, located in [golang/net/http2/server_test.go](https://github.com/golang/net/blob/ba872109ef2dc8f1da778651bd1fd3792d0e4587/http2/server_test.go#L4790)

my initial goal was to understand the vulnerability in detail, in addition to developing a tool for testing this issue at work. Other sources that were helpful include:

- Daniel Stenberg's [http2 book](https://daniel.haxx.se/http2/)
- [rfc 7540](https://www.rfc-editor.org/rfc/rfc7540)

## Testing with the included server

You can run the included `server.go` file which runs on a vulnerable version of golang.org/x/net (0.20.0). 

```shell
$ go run server.go
```

The server runs on port 8443, which the client points to by defaults. 

## Expected output

When ran against vulnerable servers, the client will be able to continue to send CONTINUATION frames for as long as you specify for the `wait` flag in seconds. The server prints its CPU usage every 2 seconds, which you will see increase rapidly as the client runs. In patched versions (0.23.0 and above), the server will close the connection once the header size limit is reached.

## Example

Run the client, creating 6 concurrent connections, calling https://localhost:8443, and sending continuation frames for 200 seconds for each connection:

```shell
$ go run client.go -time-limit 200  -connections 6 -url https://localhost:8443
```
