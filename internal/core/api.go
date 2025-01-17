package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/logger"
)

var errAPINotFound = errors.New("not found")

func interfaceIsEmpty(i interface{}) bool {
	return reflect.ValueOf(i).Kind() != reflect.Ptr || reflect.ValueOf(i).IsNil()
}

func fillStruct(dest interface{}, source interface{}) {
	rvsource := reflect.ValueOf(source).Elem()
	rvdest := reflect.ValueOf(dest)
	nf := rvsource.NumField()
	for i := 0; i < nf; i++ {
		fnew := rvsource.Field(i)
		if !fnew.IsNil() {
			f := rvdest.Elem().FieldByName(rvsource.Type().Field(i).Name)
			if f.Kind() == reflect.Ptr {
				f.Set(fnew)
			} else {
				f.Set(fnew.Elem())
			}
		}
	}
}

func generateStructWithOptionalFields(model interface{}) interface{} {
	var fields []reflect.StructField

	rt := reflect.TypeOf(model)
	nf := rt.NumField()
	for i := 0; i < nf; i++ {
		f := rt.Field(i)
		j := f.Tag.Get("json")

		if j != "-" && j != "paths" {
			fields = append(fields, reflect.StructField{
				Name: f.Name,
				Type: reflect.PtrTo(f.Type),
				Tag:  f.Tag,
			})
		}
	}

	return reflect.New(reflect.StructOf(fields)).Interface()
}

func loadConfData(ctx *gin.Context) (interface{}, error) {
	in := generateStructWithOptionalFields(conf.Conf{})
	err := json.NewDecoder(ctx.Request.Body).Decode(in)
	if err != nil {
		return nil, err
	}

	return in, err
}

func loadConfPathData(ctx *gin.Context) (interface{}, error) {
	in := generateStructWithOptionalFields(conf.PathConf{})
	err := json.NewDecoder(ctx.Request.Body).Decode(in)
	if err != nil {
		return nil, err
	}

	return in, err
}

func paginate2(itemsPtr interface{}, itemsPerPage int, page int) int {
	ritems := reflect.ValueOf(itemsPtr).Elem()

	itemsLen := ritems.Len()
	if itemsLen == 0 {
		return 0
	}

	pageCount := (itemsLen / itemsPerPage)
	if (itemsLen % itemsPerPage) != 0 {
		pageCount++
	}

	min := page * itemsPerPage
	if min >= itemsLen {
		min = itemsLen - 1
	}

	max := (page + 1) * itemsPerPage
	if max >= itemsLen {
		max = itemsLen
	}

	ritems.Set(ritems.Slice(min, max))

	return pageCount
}

func paginate(itemsPtr interface{}, itemsPerPageStr string, pageStr string) (int, error) {
	itemsPerPage := 100

	if itemsPerPageStr != "" {
		tmp, err := strconv.ParseUint(itemsPerPageStr, 10, 31)
		if err != nil {
			return 0, err
		}
		itemsPerPage = int(tmp)
	}

	page := 0

	if pageStr != "" {
		tmp, err := strconv.ParseUint(pageStr, 10, 31)
		if err != nil {
			return 0, err
		}
		page = int(tmp)
	}

	return paginate2(itemsPtr, itemsPerPage, page), nil
}

func abortWithError(ctx *gin.Context, err error) {
	if err == errAPINotFound {
		ctx.AbortWithStatus(http.StatusNotFound)
	} else {
		ctx.AbortWithStatus(http.StatusInternalServerError)
	}
}

func paramName(ctx *gin.Context) (string, bool) {
	name := ctx.Param("name")

	if len(name) < 2 || name[0] != '/' {
		return "", false
	}

	return name[1:], true
}

type apiPathManager interface {
	apiPathsList() (*apiPathsList, error)
	apiPathsGet(string) (*apiPath, error)
}

type apiHLSManager interface {
	apiMuxersList() (*apiHLSMuxersList, error)
	apiMuxersGet(string) (*apiHLSMuxer, error)
}

type apiRTSPServer interface {
	apiConnsList() (*apiRTSPConnsList, error)
	apiConnsGet(uuid.UUID) (*apiRTSPConn, error)
	apiSessionsList() (*apiRTSPSessionsList, error)
	apiSessionsGet(uuid.UUID) (*apiRTSPSession, error)
	apiSessionsKick(uuid.UUID) error
}

type apiRTMPServer interface {
	apiConnsList() (*apiRTMPConnsList, error)
	apiConnsGet(uuid.UUID) (*apiRTMPConn, error)
	apiConnsKick(uuid.UUID) error
}

type apiWebRTCManager interface {
	apiSessionsList() (*apiWebRTCSessionsList, error)
	apiSessionsGet(uuid.UUID) (*apiWebRTCSession, error)
	apiSessionsKick(uuid.UUID) error
}

type apiParent interface {
	logger.Writer
	apiConfigSet(conf *conf.Conf)
}

type api struct {
	conf          *conf.Conf
	pathManager   apiPathManager
	rtspServer    apiRTSPServer
	rtspsServer   apiRTSPServer
	rtmpServer    apiRTMPServer
	rtmpsServer   apiRTMPServer
	hlsManager    apiHLSManager
	webRTCManager apiWebRTCManager
	parent        apiParent

	httpServer *httpServer
	mutex      sync.Mutex
}

func newAPI(
	address string,
	readTimeout conf.StringDuration,
	conf *conf.Conf,
	pathManager apiPathManager,
	rtspServer apiRTSPServer,
	rtspsServer apiRTSPServer,
	rtmpServer apiRTMPServer,
	rtmpsServer apiRTMPServer,
	hlsManager apiHLSManager,
	webRTCManager apiWebRTCManager,
	parent apiParent,
) (*api, error) {
	a := &api{
		conf:          conf,
		pathManager:   pathManager,
		rtspServer:    rtspServer,
		rtspsServer:   rtspsServer,
		rtmpServer:    rtmpServer,
		rtmpsServer:   rtmpsServer,
		hlsManager:    hlsManager,
		webRTCManager: webRTCManager,
		parent:        parent,
	}

	router := gin.New()
	router.SetTrustedProxies(nil)
	router.StaticFS("/static", http.Dir(a.conf.DRVDirectory))

	mwLog := httpLoggerMiddleware(a)
	router.NoRoute(mwLog, httpServerHeaderMiddleware)
	group := router.Group("/", mwLog, httpServerHeaderMiddleware)
	group.GET("/info/get", a.onInfoGet)
	group.GET("/v2/config/get", a.onConfigGet)
	group.POST("/v2/config/set", a.onConfigSet)
	group.POST("/v2/config/paths/add/*name", a.onConfigPathsAdd)
	group.POST("/v2/config/paths/edit/*name", a.onConfigPathsEdit)
	group.POST("/v2/config/paths/remove/*name", a.onConfigPathsDelete)

	// group.POST("/v2/dvr/start/*name", a.onDVRStart)
	// group.POST("/v2/dvr/stop/*name", a.onDVRStop)
	group.POST("/v2/dvr/*name", a.onDVR)
	group.POST("/v2/snapshot/*name", a.onSnapshot)

	//httpflv
	group.GET("/v2/httpflv/enable/*name", a.onFLVEnable)

	if !interfaceIsEmpty(a.hlsManager) {
		group.GET("/v2/hlsmuxers/list", a.onHLSMuxersList)
		group.GET("/v2/hlsmuxers/get/*name", a.onHLSMuxersGet)
	}

	group.GET("/v2/paths/list", a.onPathsList)
	group.GET("/v2/paths/get/*name", a.onPathsGet)

	if !interfaceIsEmpty(a.rtspServer) {
		group.GET("/v2/rtspconns/list", a.onRTSPConnsList)
		group.GET("/v2/rtspconns/get/:id", a.onRTSPConnsGet)
		group.GET("/v2/rtspsessions/list", a.onRTSPSessionsList)
		group.GET("/v2/rtspsessions/get/:id", a.onRTSPSessionsGet)
		group.POST("/v2/rtspsessions/kick/:id", a.onRTSPSessionsKick)
	}

	if !interfaceIsEmpty(a.rtspsServer) {
		group.GET("/v2/rtspsconns/list", a.onRTSPSConnsList)
		group.GET("/v2/rtspsconns/get/:id", a.onRTSPSConnsGet)
		group.GET("/v2/rtspssessions/list", a.onRTSPSSessionsList)
		group.GET("/v2/rtspssessions/get/:id", a.onRTSPSSessionsGet)
		group.POST("/v2/rtspssessions/kick/:id", a.onRTSPSSessionsKick)
	}

	if !interfaceIsEmpty(a.rtmpServer) {
		group.GET("/v2/rtmpconns/list", a.onRTMPConnsList)
		group.GET("/v2/rtmpconns/get/:id", a.onRTMPConnsGet)
		group.POST("/v2/rtmpconns/kick/:id", a.onRTMPConnsKick)
	}

	if !interfaceIsEmpty(a.rtmpsServer) {
		group.GET("/v2/rtmpsconns/list", a.onRTMPSConnsList)
		group.GET("/v2/rtmpsconns/get/:id", a.onRTMPSConnsGet)
		group.POST("/v2/rtmpsconns/kick/:id", a.onRTMPSConnsKick)
	}

	if !interfaceIsEmpty(a.webRTCManager) {
		group.GET("/v2/webrtcsessions/list", a.onWebRTCSessionsList)
		group.GET("/v2/webrtcsessions/get/:id", a.onWebRTCSessionsGet)
		group.POST("/v2/webrtcsessions/kick/:id", a.onWebRTCSessionsKick)
	}

	var err error
	a.httpServer, err = newHTTPServer(
		address,
		readTimeout,
		"",
		"",
		router,
	)
	if err != nil {
		return nil, err
	}

	a.Log(logger.Info, "listener opened on "+address)

	return a, nil
}

func (a *api) close() {
	a.Log(logger.Info, "listener is closing")
	a.httpServer.close()
}

func (a *api) Log(level logger.Level, format string, args ...interface{}) {
	a.parent.Log(level, "[API] "+format, args...)
}

func (a *api) onConfigGet(ctx *gin.Context) {
	a.mutex.Lock()
	c := a.conf
	a.mutex.Unlock()

	ctx.JSON(http.StatusOK, c)
}

func (a *api) onConfigSet(ctx *gin.Context) {
	in, err := loadConfData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	fillStruct(newConf, in)

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

/*
*
查询信息api
Author：xgh
*/
func (a *api) onInfoGet(ctx *gin.Context) {
	a.Log(logger.Info, "visit onInfoGet api")
	data := make(map[string]interface{}) // 初始化一个空的map
	pathlist, _ := a.pathManager.apiPathsList()
	//遍历pathologist里的items,将items的信息存入data
	//定义一个item-list，保存所有的item信息
	var itemlist []map[string]interface{}
	for _, item := range pathlist.Items {
		dataItem := make(map[string]interface{})
		dataItem["name"] = item.Name
		dataItem["source"] = item.Source
		dataItem["readerCount"] = len(item.Readers)
		dataItem["readers"] = item.Readers
		//将item信息存入item-list
		itemlist = append(itemlist, dataItem)
	}
	data["items"] = itemlist
	data["pathCount"] = len(pathlist.Items)
	//将map转换为json
	ctx.JSON(http.StatusOK, data)
}
func (a *api) onConfigPathsAdd(ctx *gin.Context) {
	name, ok := paramName(ctx)
	//print(name)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	in, err := loadConfPathData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	if _, ok := newConf.Paths[name]; ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	newConfPath := &conf.PathConf{}

	// load default values
	newConfPath.UnmarshalJSON([]byte("{}"))

	fillStruct(newConfPath, in)

	newConf.Paths[name] = newConfPath

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)
	ctx.Status(http.StatusOK)
}

func (a *api) onConfigPathsEdit(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	in, err := loadConfPathData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	newConfPath, ok := newConf.Paths[name]
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	fillStruct(newConfPath, in)

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onConfigPathsDelete(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, ok := a.conf.Paths[name]; !ok {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}

	newConf := a.conf.Clone()
	delete(newConf.Paths, name)

	err := newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onPathsList(ctx *gin.Context) {
	data, err := a.pathManager.apiPathsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onPathsGet(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.pathManager.apiPathsGet(name)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPConnsList(ctx *gin.Context) {
	data, err := a.rtspServer.apiConnsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPConnsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtspServer.apiConnsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSessionsList(ctx *gin.Context) {
	data, err := a.rtspServer.apiSessionsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSessionsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtspServer.apiSessionsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSessionsKick(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	err = a.rtspServer.apiSessionsKick(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTSPSConnsList(ctx *gin.Context) {
	data, err := a.rtspsServer.apiConnsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSConnsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtspsServer.apiConnsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSSessionsList(ctx *gin.Context) {
	data, err := a.rtspsServer.apiSessionsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSSessionsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtspsServer.apiSessionsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTSPSSessionsKick(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	err = a.rtspsServer.apiSessionsKick(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTMPConnsList(ctx *gin.Context) {
	data, err := a.rtmpServer.apiConnsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTMPConnsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtmpServer.apiConnsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTMPConnsKick(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	err = a.rtmpServer.apiConnsKick(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTMPSConnsList(ctx *gin.Context) {
	data, err := a.rtmpsServer.apiConnsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTMPSConnsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.rtmpsServer.apiConnsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onRTMPSConnsKick(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	err = a.rtmpsServer.apiConnsKick(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onHLSMuxersList(ctx *gin.Context) {
	data, err := a.hlsManager.apiMuxersList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onHLSMuxersGet(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.hlsManager.apiMuxersGet(name)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onWebRTCSessionsList(ctx *gin.Context) {
	data, err := a.webRTCManager.apiSessionsList()
	if err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onWebRTCSessionsGet(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.webRTCManager.apiSessionsGet(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.JSON(http.StatusOK, data)
}

func (a *api) onWebRTCSessionsKick(ctx *gin.Context) {
	uuid, err := uuid.Parse(ctx.Param("id"))
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	err = a.webRTCManager.apiSessionsKick(uuid)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	ctx.Status(http.StatusOK)
}

func getDate() string {
	now := time.Now()
	year, month, day := now.Date()
	//fmt.Println(year, month, day)
	return fmt.Sprintf("%d%02d%02d", year, month, day)
}
func getHour() string {
	now := time.Now()
	hour, min, sec := now.Clock()
	//fmt.Println(hour, min, sec)
	return fmt.Sprintf("%02d-%02d-%02d", hour, min, sec)
}

func (a *api) onDVR(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.pathManager.apiPathsGet(name)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	type req struct {
		StartTime string `form:"startTime"`
		Duration  string `form:"duration"`
		Dir       string `form:"dir"`
		Name      string `form:"name"`
	}
	body, err := ioutil.ReadAll(ctx.Request.Body)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	var r req
	err = json.Unmarshal(body, &r)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	if r.Dir == "" {
		r.Dir = a.conf.DRVDirectory + "/" + getDate()
	}
	if r.Name == "" {
		r.Name = getHour() + ".mp4"
	}

	_, err = os.Stat(r.Dir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(r.Dir, 0755)
		if err != nil {
			abortWithError(ctx, err)
			a.Log(logger.Info, "DVR Mkdir error: "+err.Error())
			return
		}
	}
	go DVR(a, r.StartTime, r.Duration, data.Conf.Source, r.Dir, r.Name)
	type res struct {
		Location string `json:"location"`
	}
	var response res
	response.Location = r.Dir + "/" + r.Name
	ctx.JSON(http.StatusOK, response)
}

func (a *api) onSnapshot(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	data, err := a.pathManager.apiPathsGet(name)
	if err != nil {
		abortWithError(ctx, err)
		return
	}

	type req struct {
		StartTime string `form:"startTime"`
		Dir       string `form:"dir"`
		Name      string `form:"name"`
	}
	body, err := ioutil.ReadAll(ctx.Request.Body)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	var r req
	err = json.Unmarshal(body, &r)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	//fmt.Println(r.StartTime)
	if r.Dir == "" {
		r.Dir = a.conf.DRVDirectory + "/" + getDate()
	}
	//fmt.Println(r.Dir)
	if r.Name == "" {
		r.Name = getHour() + ".jpg"
	}

	_, err = os.Stat(r.Dir)
	if os.IsNotExist(err) {
		err = os.MkdirAll(r.Dir, 0755)
		if err != nil {
			abortWithError(ctx, err)
			a.Log(logger.Info, "Mkdir error: "+err.Error())
			return
		}
	}
	go Snapshot(a, r.StartTime, data.Conf.Source, r.Dir, r.Name)
	type res struct {
		Location string `json:"location"`
	}
	var response res
	response.Location = r.Dir + "/" + r.Name
	ctx.JSON(http.StatusOK, response)
}

func DVR(a *api, startTime string, dur string, stream string, dir string, name string) {
	var cmd *exec.Cmd
	if (strings.HasPrefix(stream, "rtsps://")) || (strings.HasPrefix(stream, "rtsp://")) {
		cmd = exec.Command("ffmpeg", "-rtsp_transport", "tcp", "-i", stream, "-c", "copy", "-ss", startTime, "-t", dur, dir+"/"+name)
	} else {
		cmd = exec.Command("ffmpeg", "-i", stream, "-c", "copy", "-ss", startTime, "-t", dur, dir+"/"+name)
	}
	a.Log(logger.Info, "DVR: "+dir+"/"+name+" from: "+stream)
	err := cmd.Run()
	if err != nil {
		a.Log(logger.Info, "DVR error: "+err.Error())
		output, _ := cmd.Output()
		a.Log(logger.Info, "ffmpeg output: "+string(output))
	}
	a.Log(logger.Info, "DVR: "+dir+"/"+name+" finished")
}

func Snapshot(a *api, startTime string, stream string, dir string, name string) {
	var cmd *exec.Cmd
	if (strings.HasPrefix(stream, "rtsps://")) || (strings.HasPrefix(stream, "rtsp://")) {
		cmd = exec.Command("ffmpeg", "-rtsp_transport", "tcp", "-i", stream, "-ss", startTime, "-vframes", "1", dir+"/"+name)
	} else {
		cmd = exec.Command("ffmpeg", "-i", stream, "-ss", startTime, "-vframes", "1", dir+"/"+name)
	}
	err := cmd.Run()
	output, _ := cmd.Output()
	if err != nil {
		a.Log(logger.Info, "Snapshot error: "+err.Error())
		a.Log(logger.Info, "ffmpeg output: "+string(output))
	}
	a.Log(logger.Info, "Snapshot: "+dir+"/"+name+" from: "+stream)
}

func getFLVKey(a *api, path string) string {
	client := &http.Client{}
	req, err := http.NewRequest("GET", "http://"+a.conf.FLVTransServer+a.conf.FLVAPIPort+"/control/get?room="+path, nil)
	if err != nil {
		a.Log(logger.Info, "get flv key res error: "+err.Error())
		return ""
	}

	resp, err := client.Do(req)
	if err != nil {
		a.Log(logger.Info, "get flv key resp error: "+err.Error())
		return ""
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		a.Log(logger.Info, "get flv key resp body error: "+err.Error())
		return ""
	}
	//fmt.Println(string(body))
	type res struct {
		Status int    `json:"status"`
		Data   string `json:"data"`
	}
	var response res
	err = json.Unmarshal(body, &response)
	if err != nil {
		a.Log(logger.Info, "get flv key resp json error: "+err.Error())
		return ""
	}
	if response.Status != 200 {
		a.Log(logger.Info, "get flv key resp status error: "+strconv.Itoa(response.Status))
		return ""
	}
	return response.Data
}

func FLVTrans(a *api, key string, sourcePath string) {
	cmd := exec.Command("ffmpeg", "-i", "rtmp://localhost"+a.conf.RTMPAddress+"/"+sourcePath, "-c", "copy", "-f", "flv",
		"rtmp://"+a.conf.FLVTransServer+a.conf.FLVPushPort+"/live/"+key)
	err := cmd.Run()
	if err != nil {
		a.Log(logger.Info, "FLV error: "+err.Error())
	} else {
		a.Log(logger.Info, "FLV "+"from: "+sourcePath+"finished")
	}
}

func (a *api) onFLVEnable(ctx *gin.Context) {
	name, ok := paramName(ctx)
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	_, err := a.pathManager.apiPathsGet(name)
	if err != nil {
		abortWithError(ctx, err)
		return
	}
	//fmt.Println(data.Conf.Source)

	FLVKey := getFLVKey(a, name)
	if FLVKey == "" {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	go FLVTrans(a, FLVKey, name)

	ctx.Status(http.StatusOK)
}

// confReload is called by core.
func (a *api) confReload(conf *conf.Conf) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.conf = conf
}
