package mq

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
)

func brokerImage(engine, version string) (string, error) {
	switch engine + "/" + version {
	case "RABBITMQ/4.0", "RABBITMQ/4.0.9":
		return "rabbitmq:4.0.9-management", nil
	case "RABBITMQ/4.1", "RABBITMQ/4.1.4":
		return "rabbitmq:4.1.4-management", nil
	case "RABBITMQ/3.13", "RABBITMQ/3.13.7":
		return "rabbitmq:3.13.7-management", nil
	case "ACTIVEMQ/5.17", "ACTIVEMQ/5.17.6":
		return "apache/activemq-classic:5.17.6", nil
	case "ACTIVEMQ/5.18", "ACTIVEMQ/5.18.7":
		return "apache/activemq-classic:5.18.7", nil
	case "ACTIVEMQ/5.19", "ACTIVEMQ/5.19.0":
		return "apache/activemq-classic:5.19.0", nil
	default:
		return "", fmt.Errorf("no Docker image mapping for %s %s", engine, version)
	}
}

func brokerPorts(engine string) []int64 {
	if engine == rabbitMQ {
		return []int64{5672, 15672}
	}

	return []int64{61616, 5672, 61613, 1883, 61614, 8161}
}

func brokerEndpoints(engine string, ports map[int64]int64) api.BrokerInstance {
	endpoint := func(scheme string, port int64) string {
		return fmt.Sprintf("%s://127.0.0.1:%d", scheme, ports[port])
	}

	if engine == rabbitMQ {
		return api.BrokerInstance{ConsoleURL: new(endpoint("http", 15672)), Endpoints: []string{endpoint("amqp", 5672)}}
	}

	return api.BrokerInstance{ConsoleURL: new(endpoint("http", 8161)), Endpoints: []string{
		endpoint("tcp", 61616), endpoint("amqp", 5672), endpoint("stomp", 61613),
		endpoint("mqtt", 1883), endpoint("ws", 61614),
	}}
}

// activeConfig uses XML escaping for credentials, never interpolation into a shell.
func activeConfig(users []api.User) (broker, realm string) {
	escape := func(s string) string {
		b := new(bytes.Buffer{})
		_ = xml.EscapeText(b, []byte(s))
		return b.String()
	}

	broker = `<beans
xmlns="http://www.springframework.org/schema/beans"
xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
xsi:schemaLocation="http://www.springframework.org/schema/beans
http://www.springframework.org/schema/beans/spring-beans.xsd
http://activemq.apache.org/schema/core
http://activemq.apache.org/schema/core/activemq-core.xsd">
<bean class="org.springframework.beans.factory.config.PropertyPlaceholderConfigurer">
<property name="ignoreUnresolvablePlaceholders" value="true"/>
<property name="systemPropertiesModeName" value="SYSTEM_PROPERTIES_MODE_NEVER"/>
<property name="properties"><props>
<prop key="activemq.home">/opt/apache-activemq</prop>
<prop key="activemq.conf">/opt/apache-activemq/conf</prop>
<prop key="activemq.data">/opt/apache-activemq/data</prop>
<prop key="jetty.host">0.0.0.0</prop>
<prop key="jetty.port">8161</prop>
</props></property></bean>
<broker xmlns="http://activemq.apache.org/schema/core" brokerName="localhost"
dataDirectory="${activemq.data}">
<persistenceAdapter><kahaDB directory="${activemq.data}/kahadb"/></persistenceAdapter>
<plugins><simpleAuthenticationPlugin><users>`
	for _, user := range users {
		groups := strings.Join(user.Groups, ",")

		broker += `<bean xmlns="http://www.springframework.org/schema/beans" ` +
			`class="org.apache.activemq.security.AuthenticationUser">` +
			`<constructor-arg index="0" value="` + escape(common.Value(user.Username)) + `"/>` +
			`<constructor-arg index="1" value="` + escape(springLiteral(common.Value(user.Password))) + `"/>` +
			`<constructor-arg index="2" value="` + escape(springLiteral(groups)) + `"/></bean>`

		if common.Value(user.ConsoleAccess) {
			realm += propertyEscape(
				common.Value(user.Username),
			) + ": " + propertyEscape(
				common.Value(user.Password),
			) + ", admin\n"
		}
	}

	broker += `</users></simpleAuthenticationPlugin></plugins><transportConnectors>`
	for _, connector := range []struct{ name, uri string }{
		{"openwire", "tcp://0.0.0.0:61616"},
		{"amqp", "amqp://0.0.0.0:5672"},
		{"stomp", "stomp://0.0.0.0:61613"},
		{"mqtt", "mqtt://0.0.0.0:1883"},
		{"ws", "ws://0.0.0.0:61614"},
	} {
		broker += `<transportConnector name="` + connector.name + `" uri="` + connector.uri + `"/>`
	}

	broker += `</transportConnectors></broker><import resource="jetty.xml"/></beans>`
	return broker, realm
}

// springLiteral protects user text from Spring placeholder/expression interpolation.
func springLiteral(value string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	return "#{new java.lang.String(T(java.util.Base64).getDecoder().decode('" + encoded + "'), 'UTF-8')}"
}

func propertyEscape(value string) string {
	return strings.NewReplacer("\\", "\\\\", " ", "\\ ", "#", "\\#", "!", "\\!", ":", "\\:", "=", "\\=").Replace(value)
}

func (s *Service) ensureBroker(ctx context.Context, name, volume string, r *record) (map[int64]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	image, err := brokerImage(*r.Broker.EngineType, *r.Broker.EngineVersion)
	if err != nil {
		return nil, err
	}

	_, err = dockercli.Run(ctx, nil, "inspect", "--format", "{{.Id}}", name)
	if err != nil && !dockercli.Gone(err) {
		return nil, err
	}

	if dockercli.Gone(err) {
		if len(r.Credentials) == 0 {
			return nil, fmt.Errorf("broker has no runtime credentials; recreate it in Docker mode")
		}

		identity := s.runtime.owner + "|" + *r.Broker.BrokerArn
		if _, err = dockercli.Run(ctx, nil, "volume", "create", "--label", mqLabel+"="+identity, volume); err != nil {
			return nil, err
		}

		args := []string{"create", "--name", name, "--hostname", name, "--label", mqLabel + "=" + identity}
		for _, port := range brokerPorts(*r.Broker.EngineType) {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d/tcp", port))
		}

		path := "/opt/apache-activemq/data"
		if *r.Broker.EngineType == rabbitMQ {
			path = "/var/lib/rabbitmq"
			args = append(args, "-e", "RABBITMQ_DEFAULT_USER="+*r.Credentials[0].Username,
				"-e", "RABBITMQ_DEFAULT_PASS="+*r.Credentials[0].Password,
				"-e", "RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS=+S 2:2")
		}

		args = append(args, "-v", volume+":"+path, "--", image)
		if _, err = dockercli.Run(ctx, nil, args...); err != nil {
			return nil, err
		}

		if *r.Broker.EngineType == activeMQ {
			broker, realm := activeConfig(r.Credentials)
			archive := new(bytes.Buffer{})
			writer := tar.NewWriter(archive)
			for _, file := range []struct{ name, body string }{{"activemq.xml", broker}, {"jetty-realm.properties", realm}} {
				if headerErr := writer.WriteHeader(
					&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.body))},
				); headerErr != nil {
					return nil, headerErr
				}

				if _, err = writer.Write([]byte(file.body)); err != nil {
					return nil, err
				}
			}

			if closeErr := writer.Close(); closeErr != nil {
				return nil, closeErr
			}

			if _, err = dockercli.Run(ctx, archive, "cp", "-", name+":/opt/apache-activemq/conf"); err != nil {
				_ = dockercli.RemoveContainerVolume(ctx, name, "")
				return nil, err
			}
		}
	}

	if _, err = dockercli.Run(ctx, nil, "start", name); err != nil {
		return nil, err
	}

	ports := map[int64]int64{}
	for _, port := range brokerPorts(*r.Broker.EngineType) {
		ports[port], err = dockercli.HostPort(ctx, name, port)
		if err != nil {
			return nil, err
		}
	}

	return ports, nil
}

func probeBroker(ctx context.Context, r *record, ports map[int64]int64) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	path, port := "/", int64(8161)
	if *r.Broker.EngineType == rabbitMQ {
		path, port = "/api/health/checks/alarms", 15672
	} else {
		conn, err := (&net.Dialer{}).DialContext(
			ctx,
			"tcp",
			net.JoinHostPort("127.0.0.1", strconv.FormatInt(ports[61616], 10)),
		)
		if err != nil {
			return err
		}

		_ = conn.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d%s", ports[port], path), http.NoBody)
	if err != nil {
		return err
	}

	if *r.Broker.EngineType == rabbitMQ && len(r.Credentials) > 0 {
		req.SetBasicAuth(*r.Credentials[0].Username, *r.Credentials[0].Password)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	if *r.Broker.EngineType == activeMQ &&
		(res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden) {
		return nil
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("broker readiness: HTTP %d", res.StatusCode)
	}

	return nil
}

// compatibleUpgrade permits aliases of the same image and ActiveMQ's forward 5.x KahaDB upgrades.
// RabbitMQ major/minor upgrades need feature-flag sequencing which this runtime does not perform.
func compatibleUpgrade(engine, before, after string) bool {
	oldImage, oldErr := brokerImage(engine, before)
	newImage, newErr := brokerImage(engine, after)
	if oldErr != nil || newErr != nil {
		return false
	}

	if oldImage == newImage {
		return true
	}

	if engine != activeMQ {
		return false
	}

	order := map[string]int{
		"apache/activemq-classic:5.17.6": 1,
		"apache/activemq-classic:5.18.7": 2,
		"apache/activemq-classic:5.19.0": 3,
	}

	return order[newImage] > order[oldImage]
}
