package pwd

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/play-with-docker/play-with-docker/config"
	"github.com/play-with-docker/play-with-docker/docker"
	"github.com/play-with-docker/play-with-docker/event"
	"github.com/play-with-docker/play-with-docker/pwd/types"

	"golang.org/x/text/encoding"
)

type sessionWriter struct {
	sessionId    string
	instanceName string
	event        event.EventApi
}

var terms = make(map[string]map[string]net.Conn)

func (s *sessionWriter) Write(p []byte) (n int, err error) {
	s.event.Emit(event.INSTANCE_TERMINAL_OUT, s.sessionId, s.instanceName, string(p))
	return len(p), nil
}

type InstanceConfig struct {
	ImageName  string
	Alias      string
	Hostname   string
	ServerCert []byte
	ServerKey  []byte
	CACert     []byte
	Cert       []byte
	Key        []byte
	Host       string
}

func (p *pwd) InstanceResizeTerminal(instance *types.Instance, rows, cols uint) error {
	defer observeAction("InstanceResizeTerminal", time.Now())
	return p.docker.ContainerResize(instance.Name, rows, cols)
}

func (p *pwd) InstanceAttachTerminal(instance *types.Instance) error {
	// already have a connection for this instance
	if getInstanceTermConn(instance.SessionId, instance.Name) != nil {
		return nil
	}
	conn, err := p.docker.CreateAttachConnection(instance.Name)

	if err != nil {
		return err
	}

	encoder := encoding.Replacement.NewEncoder()
	sw := &sessionWriter{sessionId: instance.Session.Id, instanceName: instance.Name, event: p.event}
	if terms[instance.SessionId] == nil {
		terms[instance.SessionId] = map[string]net.Conn{instance.Name: conn}
	} else {
		terms[instance.SessionId][instance.Name] = conn
	}
	io.Copy(encoder.Writer(sw), conn)

	return nil
}

func (p *pwd) InstanceUploadFromUrl(instance *types.Instance, fileName, dest string, url string) error {
	defer observeAction("InstanceUploadFromUrl", time.Now())
	log.Printf("Downloading file [%s]\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("Could not download file [%s]. Error: %s\n", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Could not download file [%s]. Status code: %d\n", url, resp.StatusCode)
	}

	copyErr := p.docker.CopyToContainer(instance.Name, dest, fileName, resp.Body)

	if copyErr != nil {
		return fmt.Errorf("Error while downloading file [%s]. Error: %s\n", url, copyErr)
	}

	return nil
}

func (p *pwd) getInstanceCWD(instance *types.Instance) (string, error) {
	b := bytes.NewBufferString("")

	if c, err := p.docker.ExecAttach(instance.Name, []string{"bash", "-c", `pwdx $(</var/run/cwd)`}, b); c > 0 {
		log.Println(b.String())
		return "", fmt.Errorf("Error %d trying to get CWD", c)
	} else if err != nil {
		return "", err
	}

	cwd := strings.TrimSpace(strings.Split(b.String(), ":")[1])

	return cwd, nil
}

func (p *pwd) InstanceUploadFromReader(instance *types.Instance, fileName, dest string, reader io.Reader) error {
	defer observeAction("InstanceUploadFromReader", time.Now())

	var finalDest string
	if filepath.IsAbs(dest) {
		finalDest = dest
	} else {
		if cwd, err := p.getInstanceCWD(instance); err != nil {
			return err
		} else {
			finalDest = fmt.Sprintf("%s/%s", cwd, dest)
		}
	}

	copyErr := p.docker.CopyToContainer(instance.Name, finalDest, fileName, reader)

	if copyErr != nil {
		return fmt.Errorf("Error while uploading file [%s]. Error: %s\n", fileName, copyErr)
	}

	return nil
}

func (p *pwd) InstanceGet(session *types.Session, name string) *types.Instance {
	defer observeAction("InstanceGet", time.Now())
	return session.Instances[name]
}

func (p *pwd) InstanceFindByIP(ip string) *types.Instance {
	defer observeAction("InstanceFindByIP", time.Now())
	i, err := p.storage.InstanceFindByIP(ip)
	if err != nil {
		return nil
	}

	return i
}

func (p *pwd) InstanceFindByIPAndSession(sessionPrefix, ip string) *types.Instance {
	defer observeAction("InstanceFindByIPAndSession", time.Now())
	i, err := p.storage.InstanceFindByIPAndSession(sessionPrefix, ip)
	if err != nil {
		return nil
	}

	return i
}

func (p *pwd) InstanceFindByAlias(sessionPrefix, alias string) *types.Instance {
	defer observeAction("InstanceFindByAlias", time.Now())
	i, err := p.storage.InstanceFindByAlias(sessionPrefix, alias)
	if err != nil {
		return nil
	}
	return i
}

func (p *pwd) InstanceDelete(session *types.Session, instance *types.Instance) error {
	defer observeAction("InstanceDelete", time.Now())
	conn := getInstanceTermConn(session.Id, instance.Name)
	if conn != nil {
		conn.Close()
	}
	err := p.docker.DeleteContainer(instance.Name)
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		log.Println(err)
		return err
	}

	p.event.Emit(event.INSTANCE_DELETE, session.Id, instance.Name)

	if err := p.storage.InstanceDelete(session.Id, instance.Name); err != nil {
		return err
	}

	p.setGauges()

	return nil
}

func (p *pwd) checkHostnameExists(session *types.Session, hostname string) bool {
	containerName := fmt.Sprintf("%s_%s", session.Id[:8], hostname)
	exists := false
	for _, instance := range session.Instances {
		if instance.Name == containerName {
			exists = true
			break
		}
	}
	return exists
}

func (p *pwd) InstanceNew(session *types.Session, conf InstanceConfig) (*types.Instance, error) {
	defer observeAction("InstanceNew", time.Now())
	session.Lock()
	defer session.Unlock()

	if conf.ImageName == "" {
		conf.ImageName = config.GetDindImageName()
	}
	log.Printf("NewInstance - using image: [%s]\n", conf.ImageName)

	if conf.Hostname == "" {
		var nodeName string
		for i := 1; ; i++ {
			nodeName = fmt.Sprintf("node%d", i)
			exists := p.checkHostnameExists(session, nodeName)
			if !exists {
				break
			}
		}
		conf.Hostname = nodeName
	}
	containerName := fmt.Sprintf("%s_%s", session.Id[:8], conf.Hostname)

	opts := docker.CreateContainerOpts{
		Image:         conf.ImageName,
		SessionId:     session.Id,
		PwdIpAddress:  session.PwdIpAddress,
		ContainerName: containerName,
		Hostname:      conf.Hostname,
		ServerCert:    conf.ServerCert,
		ServerKey:     conf.ServerKey,
		CACert:        conf.CACert,
		Privileged:    false,
		HostFQDN:      conf.Host,
	}

	for _, imageName := range p.InstanceAllowedImages() {
		if conf.ImageName == imageName {
			opts.Privileged = true
			break
		}
	}

	ip, err := p.docker.CreateContainer(opts)
	if err != nil {
		return nil, err
	}

	instance := &types.Instance{}
	instance.Image = opts.Image
	instance.IP = ip
	instance.SessionId = session.Id
	instance.Name = containerName
	instance.Hostname = conf.Hostname
	instance.Alias = conf.Alias
	instance.Cert = conf.Cert
	instance.Key = conf.Key
	instance.ServerCert = conf.ServerCert
	instance.ServerKey = conf.ServerKey
	instance.CACert = conf.CACert
	instance.Session = session
	// For now this condition holds through. In the future we might need a more complex logic.
	instance.IsDockerHost = opts.Privileged

	if session.Instances == nil {
		session.Instances = make(map[string]*types.Instance)
	}
	session.Instances[instance.Name] = instance

	go p.InstanceAttachTerminal(instance)

	err = p.storage.InstanceCreate(session.Id, instance)
	if err != nil {
		return nil, err
	}

	p.event.Emit(event.INSTANCE_NEW, session.Id, instance.Name, instance.IP, instance.Hostname)

	p.setGauges()

	return instance, nil
}

func (p *pwd) InstanceWriteToTerminal(sessionId, instanceName string, data string) {
	defer observeAction("InstanceWriteToTerminal", time.Now())
	conn := getInstanceTermConn(sessionId, instanceName)
	if conn != nil && len(data) > 0 {
		conn.Write([]byte(data))
	}
}

func (p *pwd) InstanceAllowedImages() []string {
	defer observeAction("InstanceAllowedImages", time.Now())

	return []string{
		config.GetDindImageName(),
		"franela/dind:overlay2-dev",
		"franela/ucp:2.4.1",
	}

}

func (p *pwd) InstanceExec(instance *types.Instance, cmd []string) (int, error) {
	defer observeAction("InstanceExec", time.Now())
	return p.docker.Exec(instance.Name, cmd)
}

func getInstanceTermConn(sessionId, instanceName string) net.Conn {
	return terms[sessionId][instanceName]
}
