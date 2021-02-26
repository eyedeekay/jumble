# jumble
I2P-only Mumble server based on a **temporary** fork of Grumble. In the future,
this will be an I2P-First, Bridgeable* Mumble server based on Grumble, if all
goes well.

`TODO: Upstream the changes in ./pkg when they're finished and/or well tested,
and when somebody has time to review them.`

## Installation

Given a PC with a Go toolchain and an I2P router that has the SAM API enabled,
download and build the software with the command:

`go get -u -v github.com/eyedeekay/jumble`

Then create a directory to hold the configuration files. For convenience sake,
we use the same directory as the Grumble server, since the goal is for the two
to be compatible and eventually share the same underlying code.

`mkdir -p ~/.grumble`

Finally, run the application:

`~/go/bin/jumble`

## Client Configuration

In a few moments, the Jumble server will generate the I2P services, and the
Base32 addresses can be found in the config directory:

**Datagram(UDP) address:** `~/.grumble/anonymous.datagram.i2p.public.txt`

**Streaming(TLS) address:** `~/.grumble/anonymous.stream.i2p.public.txt`

**Streaming(WebSocket) address:** `~/.grumble/anonymous.web.i2p.public.txt`

You can use these addresses to configure your Mumble clients.

`TODO: More detailed i2ptunnel based instructions`

`TODO: Bernard terminal client port`

`TODO: Literally any WebClient.`
