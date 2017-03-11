package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/rancher/go-rancher/client"
)

// Rancher holds the configuration parameters
type Rancher struct {
	URL         string
	AccessKey   string
	SecretKey   string
	RegistryIds []string
	client      *client.RancherClient
}

func main() {
	r := Rancher{
		URL:         os.Getenv("CATTLE_URL"),
		AccessKey:   os.Getenv("CATTLE_ACCESS_KEY"),
		SecretKey:   os.Getenv("CATTLE_SECRET_KEY"),
		RegistryIds: []string{},
	}
	rancher, err := client.NewRancherClient(&client.ClientOpts{
		Url:       r.URL,
		AccessKey: r.AccessKey,
		SecretKey: r.SecretKey,
	})
	if err != nil {
		log.Fatalf("Unable to create Rancher API client: %s\n", err)
	}
	r.client = rancher

	if ids, ok := os.LookupEnv("AWS_ECR_REGISTRY_IDS"); ok {
		r.RegistryIds = strings.Split(ids, ",")
	}

	go healthcheck()

	r.updateEcr()
	ticker := time.NewTicker(6 * time.Hour)
	for {
		<-ticker.C
		r.updateEcr()
	}
}

func (r *Rancher) updateEcr() {
	log.Println("Updating ECR Credentials")
	ecrClient := ecr.New(session.New())

	request := &ecr.GetAuthorizationTokenInput{}
	if len(r.RegistryIds) > 0 {
		request = &ecr.GetAuthorizationTokenInput{RegistryIds: aws.StringSlice(r.RegistryIds)}
	}
	resp, err := ecrClient.GetAuthorizationToken(request)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Returned from AWS GetAuthorizationToken call successfully")

	if len(resp.AuthorizationData) < 1 {
		log.Println("Request did not return authorization data")
		return
	}

	for _, data := range resp.AuthorizationData {
		r.processToken(data)
	}
}

func (r *Rancher) processToken(data *ecr.AuthorizationData) {
	bytes, err := base64.StdEncoding.DecodeString(*data.AuthorizationToken)
	if err != nil {
		log.Printf("Error decoding authorization token: %s\n", err)
		return
	}
	token := string(bytes[:len(bytes)])

	authTokens := strings.Split(token, ":")
	if len(authTokens) != 2 {
		log.Printf("Authorization token does not contain data in <user>:<password> format: %s\n", token)
		return
	}

	registryURL, err := url.Parse(*data.ProxyEndpoint)
	if err != nil {
		log.Printf("Error parsing registry URL: %s\n", err)
		return
	}

	ecrUsername := authTokens[0]
	ecrPassword := authTokens[1]
	ecrURL := registryURL.Host

	if err != nil {
		log.Printf("Failed to create rancher client: %s\n", err)
		return
	}
	registries, err := r.client.Registry.List(&client.ListOpts{})
	if err != nil {
		log.Printf("Failed to retrieve registries: %s\n", err)
		return
	}
	log.Printf("Looking for configured registry for host %s\n", ecrURL)
	for _, registry := range registries.Data {
		serverAddress, err := url.Parse(registry.ServerAddress)
		if err != nil {
			log.Printf("Failed to parse configured registry URL %s\n", registry.ServerAddress)
			break
		}
		registryHost := serverAddress.Host
		if registryHost == "" {
			registryHost = serverAddress.Path
		}
		if registryHost == ecrURL {
			credentials, err := r.client.RegistryCredential.List(&client.ListOpts{
				Filters: map[string]interface{}{
					"registryId": registry.Id,
				},
			})
			if err != nil {
				log.Printf("Failed to retrieved registry credentials for id: %s, %s\n", registry.Id, err)
				break
			}
			if len(credentials.Data) != 1 {
				log.Printf("No credentials retrieved for registry: %s\n", registry.Id)
				break
			}
			credential := credentials.Data[0]
			_, err = r.client.RegistryCredential.Update(&credential, &client.RegistryCredential{
				PublicValue: ecrUsername,
				SecretValue: ecrPassword,
			})
			if err != nil {
				log.Printf("Failed to update registry credential %s, %s\n", credential.Id, err)
			} else {
				log.Printf("Successfully updated credentials %s for registry %s; registry address: %s\n", credential.Id, registry.Id, registryHost)
			}
			return
		}
	}
	log.Printf("Failed to find configured registry to update for URL %s\n", ecrURL)
	return
}

func healthcheck() {
	listenPort := "8080"
	p, ok := os.LookupEnv("LISTEN_PORT")
	if ok {
		listenPort = p
	}
	http.HandleFunc("/ping", ping)
	log.Printf("Starting Healthcheck listener at :%s/ping\n", listenPort)
	err := http.ListenAndServe(fmt.Sprintf(":%s", listenPort), nil)
	if err != nil {
		log.Fatal("Error creating health check listener: ", err)
	}
}

func ping(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "pong!")
}
