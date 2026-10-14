package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdklambda "github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
)

func TestLambdaLocalSDK(t *testing.T) {
	for _, test := range []struct {
		name, executable, filename, code string
		runtime                          lambdatypes.Runtime
	}{
		{"python", "python3", "nested/app.py", `import os,urllib.request,time
import helper
def handler(e,c):
 print('local log')
 if e.get('fail'): raise ValueError('boom')
 if e.get('sleep'): time.sleep(10)
 with urllib.request.urlopen(os.environ['AWS_ENDPOINT_URL']+'/_tinystack/health') as response:
  status=response.status
 return {'value':e['value'],'greeting':os.environ['GREETING'],'layer':helper.value,'status':status,
 'name':c.function_name,'memory':c.memory_limit_in_mb,'client':c.client_context,
 'remaining':c.get_remaining_time_in_millis()}
`, lambdatypes.RuntimePython312},
		{"cjs", "node", "nested/app.cjs", `const helper=require('helper');
exports.handler=(e,c,cb)=>{
 console.log('local log');
 if(e.fail)throw new Error('boom');
 if(e.sleep){setTimeout(()=>cb(null,{}),10000);return;}
 fetch(process.env.AWS_ENDPOINT_URL+'/_tinystack/health').then(r=>cb(null,{value:e.value,
 greeting:process.env.GREETING,layer:helper.value,status:r.status,name:c.functionName,
 memory:c.memoryLimitInMB,client:c.clientContext,remaining:c.getRemainingTimeInMillis()})).catch(cb);
};`, lambdatypes.RuntimeNodejs22x},
		{"esm", "node", "nested/app.mjs", `import helper from 'helper';
export async function handler(e,c){
 console.log('local log');
 if(e.fail)throw new Error('boom');
 if(e.sleep)await new Promise(r=>setTimeout(r,10000));
 const response=await fetch(process.env.AWS_ENDPOINT_URL+'/_tinystack/health');
 return {value:e.value,greeting:process.env.GREETING,layer:helper.value,status:response.status,name:c.functionName,
 memory:c.memoryLimitInMB,client:c.clientContext,remaining:c.getRemainingTimeInMillis()};
}`, lambdatypes.RuntimeNodejs22x},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := exec.LookPath(test.executable); err != nil {
				t.Skip(test.executable + " is absent")
			}

			r := local.New(local.Options{})
			s := serverWith(t, r)
			c := lambdaClient(s, "test")
			layerFile, layerCode := "python/helper.py", "value='layer-value'"
			if test.executable == "node" {
				layerFile, layerCode = "nodejs/node_modules/helper/index.js", "exports.value='layer-value';"
			}

			layer, err := c.PublishLayerVersion(context.Background(), &sdklambda.PublishLayerVersionInput{
				LayerName: new(
					"local-layer",
				),
				Content: &lambdatypes.LayerVersionContentInput{ZipFile: zipOf(t, layerFile, layerCode)},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.CreateFunction(context.Background(), &sdklambda.CreateFunctionInput{
				FunctionName: new(test.name),
				Runtime:      test.runtime,
				Handler:      new("nested/app.handler"),
				Role:         new("arn:aws:iam::000000000000:role/lambda"),
				Timeout:      new(int32(1)),
				Environment:  &lambdatypes.Environment{Variables: map[string]string{"GREETING": "hello"}},
				Code: &lambdatypes.FunctionCode{
					ZipFile: zipOf(t, test.filename, test.code),
				},
				Layers: []string{*layer.LayerVersionArn},
			})
			if err != nil {
				t.Fatal(err)
			}

			response, err := c.Invoke(context.Background(), &sdklambda.InvokeInput{
				FunctionName: new(test.name),
				Payload:      []byte(`{"value":42}`), LogType: lambdatypes.LogTypeTail,
				ClientContext: new(base64.StdEncoding.EncodeToString([]byte(`{"custom":{"source":"sdk"}}`))),
			})
			if err != nil {
				t.Fatal(err)
			}

			var value map[string]any
			if response.FunctionError != nil || json.Unmarshal(response.Payload, &value) != nil ||
				value["greeting"] != "hello" ||
				value["value"] != float64(42) || value["status"] != float64(200) || value["name"] != test.name ||
				value["memory"] != "128" {
				t.Fatalf("response: %s, function error: %v", response.Payload, response.FunctionError)
			}

			if value["layer"] != "layer-value" {
				t.Fatalf("layer: %s", response.Payload)
			}

			if value["client"] == nil || value["remaining"].(float64) <= 0 {
				t.Fatalf("context: %s", response.Payload)
			}

			logs, err := base64.StdEncoding.DecodeString(aws.ToString(response.LogResult))
			if err != nil || !strings.Contains(string(logs), "local log") || strings.Contains(string(logs), "Warning") {
				t.Fatalf("logs: %s %v", logs, err)
			}

			for _, failure := range []struct{ payload, kind string }{
				{`{"fail":true}`, "boom"}, {`{"sleep":true}`, "Task timed out after 1.00 seconds"},
			} {
				response, err = c.Invoke(
					context.Background(),
					&sdklambda.InvokeInput{FunctionName: new(test.name), Payload: []byte(failure.payload)},
				)
				if err != nil || response.FunctionError == nil ||
					!strings.Contains(string(response.Payload), failure.kind) {
					t.Fatalf("failure: %+v %v", response, err)
				}
			}

			if _, err = c.Invoke(context.Background(), &sdklambda.InvokeInput{
				FunctionName: new(test.name), Payload: []byte(`{}`),
				ClientContext: new("not base64"),
			}); err == nil {
				t.Fatal("invalid ClientContext accepted")
			}
		})
	}
}

func TestLambdaLocalSQSAndHotReload(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is absent")
	}

	r := local.New(local.Options{})
	s := serverWith(t, r)
	c := lambdaClient(s, "test")
	directory := t.TempDir()
	output := filepath.Join(t.TempDir(), "delivered.json")
	handler := filepath.Join(directory, "app.py")
	code := `import json

def handler(e,c):
 with open(` + strconv.Quote(output) + `, 'w') as f: json.dump(e,f)
 return 'first'
`
	if err := os.WriteFile(handler, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := c.CreateFunction(context.Background(), &sdklambda.CreateFunctionInput{
		FunctionName: new("local-queue"), Runtime: lambdatypes.RuntimePython312, Handler: new("app.handler"),
		Role: new("arn:aws:iam::000000000000:role/lambda"),
		Code: &lambdatypes.FunctionCode{S3Bucket: new("hot-reload"), S3Key: new(directory)},
	})
	if err != nil {
		t.Fatal(err)
	}

	queues := sqsClient(s, "test")
	queue, err := queues.CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: new("local-events")})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.CreateEventSourceMapping(context.Background(), &sdklambda.CreateEventSourceMappingInput{
		FunctionName: created.FunctionArn, EventSourceArn: new("arn:aws:sqs:us-east-1:000000000000:local-events"),
		BatchSize: new(int32(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = queues.SendMessage(
		context.Background(),
		&sqs.SendMessageInput{QueueUrl: queue.QueueUrl, MessageBody: new("delivered-locally")},
	)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(6 * time.Second)
	for {
		data, readErr := os.ReadFile(output)
		if readErr == nil && strings.Contains(string(data), "delivered-locally") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("SQS not delivered: %s %v", data, readErr)
		}

		time.Sleep(20 * time.Millisecond)
	}

	response, err := c.Invoke(
		context.Background(),
		&sdklambda.InvokeInput{FunctionName: created.FunctionArn, Payload: []byte(`{}`)},
	)
	if err != nil || string(response.Payload) != `"first"` {
		t.Fatalf("first: %+v %v", response, err)
	}

	if err = os.WriteFile(handler, []byte("def handler(e,c): return 'second'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	response, err = c.Invoke(
		context.Background(),
		&sdklambda.InvokeInput{FunctionName: created.FunctionArn, Payload: []byte(`{}`)},
	)
	if err != nil || string(response.Payload) != `"second"` {
		t.Fatalf("reload: %+v %v", response, err)
	}
}
