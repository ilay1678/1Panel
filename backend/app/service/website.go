package service

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/1Panel-dev/1Panel/backend/app/dto"
	"github.com/1Panel-dev/1Panel/backend/app/model"
	"github.com/1Panel-dev/1Panel/backend/constant"
	"github.com/1Panel-dev/1Panel/backend/global"
	"github.com/1Panel-dev/1Panel/backend/utils/compose"
	"github.com/1Panel-dev/1Panel/backend/utils/files"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

type WebsiteService struct {
}

func (w WebsiteService) PageWebSite(req dto.WebSiteReq) (int64, []dto.WebSiteDTO, error) {
	var websiteDTOs []dto.WebSiteDTO
	total, websites, err := websiteRepo.Page(req.Page, req.PageSize)
	if err != nil {
		return 0, nil, err
	}
	for _, web := range websites {
		websiteDTOs = append(websiteDTOs, dto.WebSiteDTO{
			WebSite: web,
		})
	}
	return total, websiteDTOs, nil
}

func (w WebsiteService) CreateWebsite(create dto.WebSiteCreate) error {

	defaultDate, _ := time.Parse(constant.DateLayout, constant.DefaultDate)
	website := &model.WebSite{
		PrimaryDomain:  create.PrimaryDomain,
		Type:           create.Type,
		Alias:          create.Alias,
		Remark:         create.Remark,
		Status:         constant.WebRunning,
		ExpireDate:     defaultDate,
		AppInstallID:   create.AppInstallID,
		WebSiteGroupID: create.WebSiteGroupID,
		Protocol:       constant.ProtocolHTTP,
	}

	if create.Type == "deployment" {
		if create.AppType == dto.NewApp {
			install, err := ServiceGroupApp.Install(create.AppInstall.Name, create.AppInstall.AppDetailId, create.AppInstall.Params)
			if err != nil {
				return err
			}
			website.AppInstallID = install.ID
		}
	} else {
		if err := createStaticHtml(website); err != nil {
			return err
		}
	}

	tx, ctx := getTxAndContext()
	if err := websiteRepo.Create(ctx, website); err != nil {
		return err
	}
	var domains []model.WebSiteDomain
	domains = append(domains, model.WebSiteDomain{Domain: website.PrimaryDomain, WebSiteID: website.ID, Port: 80})

	otherDomainArray := strings.Split(create.OtherDomains, "\n")
	for _, domain := range otherDomainArray {
		if domain == "" {
			continue
		}
		domainModel, err := getDomain(domain, website.ID)
		if err != nil {
			tx.Rollback()
			return err
		}
		if reflect.DeepEqual(domainModel, model.WebSiteDomain{}) {
			continue
		}
		domains = append(domains, domainModel)
	}
	if len(domains) > 0 {
		if err := websiteDomainRepo.BatchCreate(ctx, domains); err != nil {
			tx.Rollback()
			return err
		}
	}

	if err := configDefaultNginx(website, domains); err != nil {
		tx.Rollback()
		return err
	}

	tx.Commit()
	return nil
}

func (w WebsiteService) Backup(id uint) error {
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(id))
	if err != nil {
		return err
	}
	app, err := appInstallRepo.GetFirst(commonRepo.WithByID(website.AppInstallID))
	if err != nil {
		return err
	}
	resource, err := appInstallResourceRepo.GetFirst(appInstallResourceRepo.WithAppInstallId(website.AppInstallID))
	if err != nil {
		return err
	}
	mysqlInfo, err := appInstallRepo.LoadBaseInfoByKey(resource.Key)
	if err != nil {
		return err
	}
	nginxInfo, err := appInstallRepo.LoadBaseInfoByKey("nginx")
	if err != nil {
		return err
	}
	db, err := mysqlRepo.Get(commonRepo.WithByID(resource.ResourceId))
	if err != nil {
		return err
	}
	localDir, err := loadLocalDir()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s_%s", website.PrimaryDomain, time.Now().Format("20060102150405"))
	backupDir := fmt.Sprintf("website/%s/%s", website.PrimaryDomain, name)
	fullDir := fmt.Sprintf("%s/%s", localDir, backupDir)
	if _, err := os.Stat(fullDir); err != nil && os.IsNotExist(err) {
		if err = os.MkdirAll(fullDir, os.ModePerm); err != nil {
			if err != nil {
				return fmt.Errorf("mkdir %s failed, err: %v", fullDir, err)
			}
		}
	}
	nginxConfFile := fmt.Sprintf("%s/nginx/%s/conf/conf.d/%s.conf", constant.AppInstallDir, nginxInfo.Name, website.PrimaryDomain)
	src, err := os.OpenFile(nginxConfFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0775)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.Create(fmt.Sprintf("%s/%s.conf", fullDir, website.PrimaryDomain))
	if err != nil {
		return err
	}
	defer out.Close()
	_, _ = io.Copy(out, src)

	if website.Type == "deployment" {
		dbFile := fmt.Sprintf("%s/%s.sql", fullDir, website.PrimaryDomain)
		outfile, _ := os.OpenFile(dbFile, os.O_RDWR|os.O_CREATE, 0755)
		cmd := exec.Command("docker", "exec", mysqlInfo.ContainerName, "mysqldump", "-uroot", "-p"+mysqlInfo.Password, db.Name)
		cmd.Stdout = outfile
		_ = cmd.Run()
		_ = cmd.Wait()

		websiteDir := fmt.Sprintf("%s/%s/%s", constant.AppInstallDir, app.App.Key, app.Name)
		if err := handleTar(websiteDir, fullDir, fmt.Sprintf("%s.web.tar.gz", website.PrimaryDomain), ""); err != nil {
			return err
		}
	} else {
		websiteDir := fmt.Sprintf("%s/nginx/%s/www/%s", constant.AppInstallDir, nginxInfo.Name, website.PrimaryDomain)
		if err := handleTar(websiteDir, fullDir, fmt.Sprintf("%s.web.tar.gz", website.PrimaryDomain), ""); err != nil {
			return err
		}
	}
	tarDir := fmt.Sprintf("%s/website/%s", localDir, website.PrimaryDomain)
	tarName := fmt.Sprintf("%s.tar.gz", name)
	itemDir := strings.ReplaceAll(fullDir[strings.LastIndex(fullDir, "/"):], "/", "")
	aheadDir := strings.ReplaceAll(fullDir, itemDir, "")
	tarcmd := exec.Command("tar", "zcvf", fullDir+".tar.gz", "-C", aheadDir, itemDir)
	stdout, err := tarcmd.CombinedOutput()
	if err != nil {
		return errors.New(string(stdout))
	}
	_ = os.RemoveAll(fullDir)

	record := &model.BackupRecord{
		Type:       "website-" + website.Type,
		Name:       website.PrimaryDomain,
		DetailName: "",
		Source:     "LOCAL",
		BackupType: "LOCAL",
		FileDir:    tarDir,
		FileName:   tarName,
	}
	if err := backupRepo.CreateRecord(record); err != nil {
		global.LOG.Errorf("save backup record failed, err: %v", err)
	}
	return nil
}

func (w WebsiteService) Recover(req dto.WebSiteRecover) error {
	website, err := websiteRepo.GetFirst(websiteRepo.WithByDomain(req.WebsiteName))
	if err != nil {
		return err
	}
	app, err := appInstallRepo.GetFirst(commonRepo.WithByID(website.AppInstallID))
	if err != nil {
		return err
	}
	if !strings.Contains(req.BackupName, "/") {
		return errors.New("error path of request")
	}
	fileDir := req.BackupName[:strings.LastIndex(req.BackupName, "/")]
	fileName := strings.ReplaceAll(req.BackupName[strings.LastIndex(req.BackupName, "/"):], ".tar.gz", "")
	if err := handleUnTar(req.BackupName, fileDir); err != nil {
		return err
	}
	fileDir = fileDir + "/" + fileName
	resource, err := appInstallResourceRepo.GetFirst(appInstallResourceRepo.WithAppInstallId(website.AppInstallID))
	if err != nil {
		return err
	}
	nginxInfo, err := appInstallRepo.LoadBaseInfoByKey("nginx")
	if err != nil {
		return err
	}
	mysqlInfo, err := appInstallRepo.LoadBaseInfoByKey("mysql")
	if err != nil {
		return err
	}
	db, err := mysqlRepo.Get(commonRepo.WithByID(resource.ResourceId))
	if err != nil {
		return err
	}
	src, err := os.OpenFile(fmt.Sprintf("%s/%s.conf", fileDir, website.PrimaryDomain), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0775)
	if err != nil {
		return err
	}
	defer src.Close()
	var out *os.File
	nginxConfDir := fmt.Sprintf("%s/nginx/%s/conf/conf.d/%s.conf", constant.AppInstallDir, nginxInfo.Name, website.PrimaryDomain)
	if _, err := os.Stat(nginxConfDir); err != nil {
		out, err = os.Create(fmt.Sprintf("%s/%s.conf", nginxConfDir, website.PrimaryDomain))
		if err != nil {
			return err
		}
	} else {
		out, err = os.OpenFile(nginxConfDir, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
	}
	defer out.Close()
	_, _ = io.Copy(out, src)
	if website.Type == "deployment" {
		cmd := exec.Command("docker", "exec", "-i", mysqlInfo.ContainerName, "mysql", "-uroot", "-p"+mysqlInfo.Password, db.Name)
		sql, err := os.OpenFile(fmt.Sprintf("%s/%s.sql", fileDir, website.PrimaryDomain), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
		if err != nil {
			return err
		}
		cmd.Stdin = sql
		stdout, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(string(stdout))
		}

		appDir := fmt.Sprintf("%s/%s", constant.AppInstallDir, app.App.Key)
		if err := handleUnTar(fmt.Sprintf("%s/%s.web.tar.gz", fileDir, website.PrimaryDomain), appDir); err != nil {
			return err
		}
		if _, err := compose.Restart(fmt.Sprintf("%s/%s/docker-compose.yml", appDir, app.Name)); err != nil {
			return err
		}
	} else {
		appDir := fmt.Sprintf("%s/nginx/%s/www", constant.AppInstallDir, nginxInfo.Name)
		if err := handleUnTar(fmt.Sprintf("%s/%s.web.tar.gz", fileDir, website.PrimaryDomain), appDir); err != nil {
			return err
		}
	}
	cmd := exec.Command("docker", "exec", "-i", nginxInfo.ContainerName, "nginx", "-s", "reload")
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(stdout))
	}
	return nil
}

func (w WebsiteService) UpdateWebsite(req dto.WebSiteUpdate) error {
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(req.ID))
	if err != nil {
		return err
	}
	website.PrimaryDomain = req.PrimaryDomain
	website.WebSiteGroupID = req.WebSiteGroupID
	website.Remark = req.Remark

	return websiteRepo.Save(context.TODO(), &website)
}

func (w WebsiteService) GetWebsite(id uint) (dto.WebsiteDTO, error) {
	var res dto.WebsiteDTO
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(id))
	if err != nil {
		return res, err
	}
	res.WebSite = website
	return res, nil
}

func (w WebsiteService) DeleteWebSite(req dto.WebSiteDel) error {

	website, err := websiteRepo.GetFirst(commonRepo.WithByID(req.ID))
	if err != nil {
		return err
	}
	if err := delNginxConfig(website); err != nil {
		return err
	}
	tx, ctx := getTxAndContext()

	if req.DeleteApp {
		websites, _ := websiteRepo.GetBy(websiteRepo.WithAppInstallId(website.AppInstallID))
		if len(websites) > 1 {
			return errors.New("other website use this app")
		}
		appInstall, err := appInstallRepo.GetFirst(commonRepo.WithByID(website.AppInstallID))
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if !reflect.DeepEqual(model.AppInstall{}, appInstall) {
			if err := deleteAppInstall(ctx, appInstall); err != nil {
				return err
			}
		}
	}
	//TODO 删除备份
	if err := websiteRepo.DeleteBy(ctx, commonRepo.WithByID(req.ID)); err != nil {
		tx.Rollback()
		return err
	}
	if err := websiteDomainRepo.DeleteBy(ctx, websiteDomainRepo.WithWebSiteId(req.ID)); err != nil {
		tx.Rollback()
		return err
	}
	tx.Commit()
	return nil
}

func (w WebsiteService) CreateWebsiteDomain(create dto.WebSiteDomainCreate) (model.WebSiteDomain, error) {
	var domainModel model.WebSiteDomain
	var ports []int
	var domains []string

	website, err := websiteRepo.GetFirst(commonRepo.WithByID(create.WebSiteID))
	if err != nil {
		return domainModel, err
	}
	if oldDomains, _ := websiteDomainRepo.GetBy(websiteDomainRepo.WithWebSiteId(create.WebSiteID), websiteDomainRepo.WithPort(create.Port)); len(oldDomains) == 0 {
		ports = append(ports, create.Port)
	}
	domains = append(domains, create.Domain)
	if err := addListenAndServerName(website, ports, domains); err != nil {
		return domainModel, err
	}
	domainModel = model.WebSiteDomain{
		Domain:    create.Domain,
		Port:      create.Port,
		WebSiteID: create.WebSiteID,
	}
	return domainModel, websiteDomainRepo.Create(context.TODO(), &domainModel)
}

func (w WebsiteService) GetWebsiteDomain(websiteId uint) ([]model.WebSiteDomain, error) {
	return websiteDomainRepo.GetBy(websiteDomainRepo.WithWebSiteId(websiteId))
}

func (w WebsiteService) DeleteWebsiteDomain(domainId uint) error {

	webSiteDomain, err := websiteDomainRepo.GetFirst(commonRepo.WithByID(domainId))
	if err != nil {
		return err
	}

	if websiteDomains, _ := websiteDomainRepo.GetBy(websiteDomainRepo.WithWebSiteId(webSiteDomain.WebSiteID)); len(websiteDomains) == 1 {
		return fmt.Errorf("can not delete last domain")
	}
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(webSiteDomain.WebSiteID))
	if err != nil {
		return err
	}
	var ports []int
	if oldDomains, _ := websiteDomainRepo.GetBy(websiteDomainRepo.WithWebSiteId(webSiteDomain.WebSiteID), websiteDomainRepo.WithPort(webSiteDomain.Port)); len(oldDomains) == 1 {
		ports = append(ports, webSiteDomain.Port)
	}

	var domains []string
	if oldDomains, _ := websiteDomainRepo.GetBy(websiteDomainRepo.WithWebSiteId(webSiteDomain.WebSiteID), websiteDomainRepo.WithDomain(webSiteDomain.Domain)); len(oldDomains) == 1 {
		domains = append(domains, webSiteDomain.Domain)
	}
	if len(ports) > 0 || len(domains) > 0 {
		if err := deleteListenAndServerName(website, ports, domains); err != nil {
			return err
		}
	}

	return websiteDomainRepo.DeleteBy(context.TODO(), commonRepo.WithByID(domainId))
}

func (w WebsiteService) GetNginxConfigByScope(req dto.NginxConfigReq) ([]dto.NginxParam, error) {

	keys, ok := dto.ScopeKeyMap[req.Scope]
	if !ok || len(keys) == 0 {
		return nil, nil
	}

	website, err := websiteRepo.GetFirst(commonRepo.WithByID(req.WebSiteID))
	if err != nil {
		return nil, err
	}

	return getNginxConfigByKeys(website, keys)
}

func (w WebsiteService) UpdateNginxConfigByScope(req dto.NginxConfigReq) error {

	keys, ok := dto.ScopeKeyMap[req.Scope]
	if !ok || len(keys) == 0 {
		return nil
	}
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(req.WebSiteID))
	if err != nil {
		return err
	}
	if req.Operate == dto.ConfigDel {
		return deleteNginxConfig(website, keys)
	}

	return updateNginxConfig(website, getNginxParams(req.Params, keys), req.Scope)
}

func (w WebsiteService) GetWebsiteNginxConfig(websiteId uint) (dto.FileInfo, error) {
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(websiteId))
	if err != nil {
		return dto.FileInfo{}, err
	}

	nginxApp, err := appRepo.GetFirst(appRepo.WithKey("nginx"))
	if err != nil {
		return dto.FileInfo{}, err
	}
	nginxInstall, err := appInstallRepo.GetFirst(appInstallRepo.WithAppId(nginxApp.ID))
	if err != nil {
		return dto.FileInfo{}, err
	}

	configPath := path.Join(constant.AppInstallDir, "nginx", nginxInstall.Name, "conf", "conf.d", website.Alias+".conf")

	info, err := files.NewFileInfo(files.FileOption{
		Path:   configPath,
		Expand: true,
	})
	if err != nil {
		return dto.FileInfo{}, err
	}
	return dto.FileInfo{FileInfo: *info}, nil
}

func (w WebsiteService) GetWebsiteHTTPS(websiteId uint) (dto.WebsiteHTTPS, error) {
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(websiteId))
	if err != nil {
		return dto.WebsiteHTTPS{}, err
	}
	var res dto.WebsiteHTTPS
	if website.WebSiteSSLID == 0 {
		res.Enable = false
		return res, nil
	}
	websiteSSL, err := websiteSSLRepo.GetFirst(commonRepo.WithByID(website.WebSiteSSLID))
	if err != nil {
		return dto.WebsiteHTTPS{}, err
	}
	res.SSL = websiteSSL
	res.Enable = true
	return res, nil
}

func (w WebsiteService) OpWebsiteHTTPS(req dto.WebsiteHTTPSOp) (dto.WebsiteHTTPS, error) {
	website, err := websiteRepo.GetFirst(commonRepo.WithByID(req.WebsiteID))
	if err != nil {
		return dto.WebsiteHTTPS{}, err
	}

	var (
		res        dto.WebsiteHTTPS
		websiteSSL model.WebSiteSSL
	)
	res.Enable = req.Enable

	if req.Type == dto.SSLExisted {
		websiteSSL, err = websiteSSLRepo.GetFirst(commonRepo.WithByID(req.WebsiteSSLID))
		if err != nil {
			return dto.WebsiteHTTPS{}, err
		}
		website.WebSiteSSLID = websiteSSL.ID
		if err := websiteRepo.Save(context.TODO(), &website); err != nil {
			return dto.WebsiteHTTPS{}, err
		}
		res.SSL = websiteSSL
	}

	if req.Type == dto.Manual {
		certBlock, _ := pem.Decode([]byte(req.Certificate))
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			return dto.WebsiteHTTPS{}, err
		}
		websiteSSL.ExpireDate = cert.NotAfter
		websiteSSL.StartDate = cert.NotBefore
		websiteSSL.Type = cert.Issuer.CommonName
		websiteSSL.Organization = cert.Issuer.Organization[0]
		websiteSSL.PrimaryDomain = cert.Subject.CommonName
		if len(cert.Subject.Names) > 0 {
			var domains []string
			for _, name := range cert.Subject.Names {
				if v, ok := name.Value.(string); ok {
					if v != cert.Subject.CommonName {
						domains = append(domains, v)
					}
				}
			}
			if len(domains) > 0 {
				websiteSSL.Domains = strings.Join(domains, "")
			}
		}

		websiteSSL.Provider = dto.Manual
		websiteSSL.PrivateKey = req.PrivateKey
		websiteSSL.Pem = req.Certificate

		res.SSL = websiteSSL
	}

	if req.Enable {
		website.Protocol = constant.ProtocolHTTPS
		if err := applySSL(website, websiteSSL); err != nil {
			return dto.WebsiteHTTPS{}, err
		}
	} else {
		website.Protocol = constant.ProtocolHTTP
		website.WebSiteSSLID = 0

		if err := deleteListenAndServerName(website, []int{443}, []string{}); err != nil {
			return dto.WebsiteHTTPS{}, err
		}

		if err := deleteNginxConfig(website, getKeysFromStaticFile(dto.SSL)); err != nil {
			return dto.WebsiteHTTPS{}, err
		}
	}

	tx, ctx := getTxAndContext()
	if websiteSSL.ID == 0 {
		if err := websiteSSLRepo.Create(ctx, &websiteSSL); err != nil {
			return dto.WebsiteHTTPS{}, err
		}
		website.WebSiteSSLID = websiteSSL.ID
	}
	if err := websiteRepo.Save(ctx, &website); err != nil {
		return dto.WebsiteHTTPS{}, err
	}

	tx.Commit()
	return res, nil
}