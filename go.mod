module github.com/eyedeekay/jumble

go 1.15

replace github.com/mumble-voip/grumble v0.0.0-20200416032001-6f8c2bf2f51f => github.com/eyedeekay/grumble v0.0.0-20201227041312-50b7729bd8a5

require (
	github.com/eyedeekay/sam3 v0.32.33-0.20210226155219-0f604e88b235
	github.com/golang/protobuf v1.4.3
	github.com/gorilla/websocket v1.4.2
	golang.org/x/crypto v0.0.0-20201221181555-eec23a3978ad
	golang.org/x/sys v0.0.0-20201223074533-0d417f636930 // indirect
	google.golang.org/protobuf v1.25.0 // indirect
)
