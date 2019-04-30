package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/barnybug/cli53"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// Response is of type APIGatewayProxyResponse since we're leveraging the
// AWS Lambda Proxy Request functionality (default behavior)
//
// https://serverless.com/framework/docs/providers/aws/events/apigateway/#lambda-proxy-integration
type Response events.APIGatewayProxyResponse

// Handler is our lambda handler invoked by the `lambda.Start` function call
func Handler(ctx context.Context) () {
	err := export()
	if err != nil {
		log.Fatal(err)
	}
}

type EmailNotificationConfig struct {
	From string
	To   string
}

type S3LocationConfig struct {
	Bucket string
	Region string
}

type ZoneConfig struct {
	Name string
	Id   string
}

type Config struct {
	Region            string
	EmailNotification EmailNotificationConfig
	S3Location        S3LocationConfig
	Zone              ZoneConfig
}

func loadConfig() (Config, error) {
	var c Config
	c.Region = os.Getenv("AWS_REGION")
	if len(c.Region) == 0 {
		return c, errors.New(fmt.Sprintf("bug: AWS_REGION environment variable is not set"))
	}
	destBucketName := os.Getenv("DESTINATION_S3_BUCKET_NAME")
	if len(destBucketName) == 0 {
		return c, errors.New(fmt.Sprintf("bug: DESTINATION_S3_BUCKET_NAME environment variable is not set"))
	}
	c.S3Location.Bucket = destBucketName
	destBucketRegion := os.Getenv("DESTINATION_S3_BUCKET_REGION")
	if len(destBucketName) == 0 {
		c.S3Location.Region = c.Region
	} else {
		c.S3Location.Region = destBucketRegion
	}

	notificationEmailSender := os.Getenv("NOTIFICATION_EMAIL_SENDER")
	notificationEmailReceiver := os.Getenv("NOTIFICATION_EMAIL_RECEIVER")
	if len(notificationEmailReceiver) == 0 && len(notificationEmailSender) == 0 {
		return c, errors.New(fmt.Sprintf("bug: Both sender and receiver email id must be provided via environment variables. NOTIFICATION_EMAIL_SENDER, NOTIFICATION_EMAIL_RECEIVER"))
	}
	c.EmailNotification.From = notificationEmailSender
	c.EmailNotification.To = notificationEmailReceiver

	zonename := os.Getenv("HOSTEDZONE_NAME")
	zoneid := os.Getenv("HOSTEDZONE_ID")
	if !strings.Contains(zoneid, "/hostedzone/") {
		zoneid = fmt.Sprintf("/hostedzone/%s", zoneid)
	}
	if len(zonename) == 0 && len(zoneid) == 0 {
		return c, errors.New(fmt.Sprintf("bug: Either name or id of hosted zone must be provided via environment variables. HOSTEDZONE_NAME, HOSTEDZONE_ID"))
	}
	c.Zone.Name = zonename
	c.Zone.Id = zoneid

	return c, nil
}

func backupTimestamp(key string) (int64) {
	index := strings.LastIndex(key, "-")
	if index == -1 {
		return 0
	}
	if index == len(key)-1 {
		return 0
	}
	ts, err := strconv.ParseInt(key[index+1:len(key)-1], 10, 64)
	if err != nil {
		return 0
	}
	return ts

}

func recentBackup(c Config, sess *session.Session, domainName string) (string, error) {
	s3Client := s3.New(sess)
	var allObjects []*s3.Object
	s3Client.ListObjectsPages(&s3.ListObjectsInput{Bucket: &c.S3Location.Bucket}, func(output *s3.ListObjectsOutput, b bool) bool {
		allObjects = append(allObjects, output.Contents...)
		return true
	})

	fileprefix := fmt.Sprintf("r53-%s", domainName)
	var backupObjects []*s3.Object
	for _, o := range allObjects {
		if strings.HasPrefix(*o.Key, fileprefix) {
			backupObjects = append(backupObjects, o)
		}
	}
	if backupObjects == nil || len(backupObjects) == 0 {
		return "", nil
	}
	sort.Slice(backupObjects, func(i, j int) bool {
		return backupTimestamp(*backupObjects[i].Key) > backupTimestamp(*backupObjects[j].Key)
	})
	r, err := s3Client.GetObject(&s3.GetObjectInput{Key: backupObjects[0].Key, Bucket: &c.S3Location.Bucket})
	if err != nil {
		return "", err
	}
	d, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	return string(d), nil
}

func entries(bind string) []dns.RR {
	var records []dns.RR
	zp := dns.NewZoneParser(strings.NewReader(bind), "", "")
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		records = append(records, rr)
	}
	sort.Slice(records, func(i, j int) bool {
		return strings.Compare(records[i].String(), records[j].String()) > 0
	})
	return records
}

func hasChanged(previousBackup, currentBackup string) (bool) {
	prevRecords := entries(previousBackup)
	currentRecords := entries(currentBackup)

	if len(prevRecords) != len(currentRecords) {
		return true;
	}
	for i, pr := range prevRecords {
		if strings.Compare(pr.String(), currentRecords[i].String()) != 0 {
			return true
		}
	}
	return false
}

func changes(previousBackup, currentBackup string) ([]string) {
	var changedRecords []string
	prevRecords := entries(previousBackup)
	currentRecords := entries(currentBackup)
	existing := make(map[string]bool)
	for _, r := range prevRecords {
		existing[r.String()] = true
	}
	for _, r := range currentRecords {
		_, e := existing[r.String()]
		if !e {
			changedRecords = append(changedRecords, r.String())
		}
	}
	return changedRecords
}

func notify(config Config, sesClient *ses.SES, message string, header string) error {
	notificationDestination := new(ses.Destination)
	notificationDestination.ToAddresses = []*string{&config.EmailNotification.To}
	_, err := sesClient.SendEmail(&ses.SendEmailInput{Destination: notificationDestination, Source: &config.EmailNotification.From, Message: &ses.Message{Body: &ses.Body{Text: &ses.Content{Charset: aws.String("UTF-8"), Data: &message}}, Subject: &ses.Content{Charset: aws.String("UTF-8"), Data: &header}}})
	return err
}

func export() (error) {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	sess, err := session.NewSession(&aws.Config{Region: &config.Region})
	if err != nil {
		return err
	}
	r53Client := route53.New(sess)
	s3Client := s3.New(sess)
	sesClient := ses.New(sess)
	var hostedZones []*route53.HostedZone
	err = r53Client.ListHostedZonesPages(&route53.ListHostedZonesInput{}, func(output *route53.ListHostedZonesOutput, b bool) bool {
		hostedZones = append(hostedZones, output.HostedZones...)
		return true
	})
	if err != nil {
		return err
	}
	for _, hostedZone := range hostedZones {
		if strings.Compare(config.Zone.Name, *hostedZone.Name) == 0 || strings.Compare(config.Zone.Id, *hostedZone.Id) == 0 {
			previousBackup, err := recentBackup(config, sess, *hostedZone.Name)
			if err != nil {
				return err
			}
			buffer := new(bytes.Buffer)
			cli53.ExportBindToWriter(r53Client, hostedZone, true, buffer)
			filename := fmt.Sprintf("r53-%s-%d", *hostedZone.Name, int32(time.Now().Unix()))
			if hasChanged(previousBackup, string(buffer.Bytes())) {
				_, err = s3Client.PutObject(&s3.PutObjectInput{Body: bytes.NewReader(buffer.Bytes()), Bucket: &config.S3Location.Bucket, Key: &filename})
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("issue: failed to upload backup to bucket %s key %s", config.S3Location.Bucket, filename))
				}
				notificationMessage := fmt.Sprintf("The following records have been updated since the last backup \n %s", strings.Join(changes(previousBackup, string(buffer.Bytes())), "\n"))
				fmt.Println(notificationMessage)
				err = notify(config, sesClient, notificationMessage, fmt.Sprintf("ROUTE53 BACKUP FOR DOMAIN %s", *hostedZone.Name))
				if err != nil {
					return errors.Wrap(err, fmt.Sprintf("issue: failed to send notification email to %s", config.EmailNotification.To))
				}
			} else {
				log.Println("No Changes detected. Skipping backup")
			}
		}
	}
	return nil
}

func main() {
	lambda.Start(Handler)
}
