package kmsplugin

import (
	"bytes"
	"text/template"
)

type awsProviderTemplate struct {
	TargetHash      string
	TargetNamespace string
	ProviderImage   string
	KeyARN          string
	Region          string
	Listen          string
}

func GenerateAWSProviderTemplate(targetHash, targetNamespace, image, keyARN, region, listen string) (string, error) {
	rawAWSProviderManifest, err := asset("assets/aws-encryption-provider-pod.yaml")
	if err != nil {
		return "", err
	}

	tmplVal := awsProviderTemplate{
		TargetHash:      targetHash,
		TargetNamespace: targetNamespace,
		ProviderImage:   image,
		KeyARN:          keyARN,
		Region:          region,
		Listen:          listen,
	}
	tmpl, err := template.New("aws-provider").Parse(string(rawAWSProviderManifest))
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, tmplVal)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}
