package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"./rtorrent"
	"./scgi"

	jwt "github.com/dgrijalva/jwt-go"
	"gopkg.in/gin-gonic/gin.v1"
)

type CommandLineArgs struct {
	Port    uint
	Host    string
	RPCSock string
}

type LoginInfo struct {
	Username string
	Password string
}

const symbols = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*()_+=-/\\"

var rtConn *rtorrent.RtClient
var jwtSigningKey []byte
var loginInfo LoginInfo

func RandKey(n int) (b []byte) {
	b = make([]byte, n)
	for i := range b {
		b[i] = symbols[rand.Intn(len(symbols))]
	}
	return
}

func ValidateToken(tokenHeader string) bool {
	if len(loginInfo.Password) == 0 {
		return true
	}
	if len(tokenHeader) > 0 {
		token, err := jwt.ParseWithClaims(tokenHeader, &jwt.StandardClaims{}, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("Unexpected signing method %v", token.Header["alg"])
			}
			return jwtSigningKey, nil
		})
		if err == nil {
			if claims, ok := token.Claims.(*jwt.StandardClaims); ok && token.Valid {
				return claims.Id == loginInfo.Username && claims.Issuer == "gogurt"
			}
		} else {
			log.Println("error occured when validating token:", err)
		}
	}
	return false
}

func ReplyCheckError(c *gin.Context, err error) {
	if err == nil {
		c.JSON(http.StatusOK, gin.H{"status": "OK"})
	} else {
		c.JSON(http.StatusBadRequest, err)
	}
}

func List(c *gin.Context) {
	torrents, _ := rtConn.GetList(c.Param("view"))
	c.JSON(http.StatusOK, torrents)
}

func Index(c *gin.Context) {
	c.Redirect(http.StatusMovedPermanently, "/ui/")
}

func AddNew(c *gin.Context) {
	file, _, err := c.Request.FormFile("fileInput")
	if err == nil {
		fileData, fileErr := ioutil.ReadAll(file)
		if fileErr == nil {
			tag := c.PostForm("fileTag")
			destination := c.PostForm("destinationDir")
			err = rtConn.LoadRaw(fileData, tag, destination)
		} else {
			err = fileErr
		}
	}

	ReplyCheckError(c, err)
}

func DoAction(c *gin.Context) {
	action := c.Param("action")
	hash := c.Param("hash")
	var err error
	switch action {
	case "start":
		err = rtConn.Start(hash)
	case "stop":
		err = rtConn.Stop(hash)
	case "priorities":
		var prios []int
		c.BindJSON(&prios)
		for index, prio := range prios {
			err = rtConn.SetPriority(hash, index, prio)
			if err != nil {
				log.Println(err)
			}
		}
		err = rtConn.UpdatePriorities(hash)
	}
	ReplyCheckError(c, err)
}

func Delete(c *gin.Context) {
	hash := c.Param("hash")
	err := rtConn.Erase(hash)
	ReplyCheckError(c, err)
}

func Protected(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		authToken := c.Request.Header.Get("Authorization")
		if ValidateToken(authToken) {
			handler(c)
		} else {
			c.JSON(http.StatusUnauthorized, gin.H{"status": "invalid authorization"})
		}
	}
}

func Login(c *gin.Context) {
	login := c.PostForm("login")
	pass := c.PostForm("password")
	if login == loginInfo.Username && pass == loginInfo.Password {
		claims := &jwt.StandardClaims{
			ExpiresAt: time.Now().AddDate(0, 0, 1).Unix(),
			Id:        loginInfo.Username,
			Issuer:    "gogurt",
		}
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		if authToken, err := token.SignedString(jwtSigningKey); err == nil {
			c.JSON(http.StatusOK, gin.H{"token": authToken})
		} else {
			log.Println("error occured:", err)
		}
	} else {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "wrong login/password"})
	}
	return
}

func Token(c *gin.Context) {
	authToken := c.Request.Header.Get("Authorization")
	if ValidateToken(authToken) {
		c.JSON(http.StatusOK, gin.H{"status": "OK"})
	} else {
		c.JSON(http.StatusUnauthorized, gin.H{"status": "missing or expired authorization"})
	}
}

func ServerInfo(c *gin.Context) {
	free, total := GetDiskSpace()
	c.JSON(http.StatusOK, gin.H{"space": gin.H{"free": free, "total": total}})
}

func Details(c *gin.Context) {
	hash := c.Param("hash")
	// reduce calls to a multicall
	location, err := rtConn.GetDirectory(hash)
	files, err := rtConn.GetFiles(hash)
	tags, err := rtConn.GetTags(hash)
	if err == nil {
		sort.Sort(rtorrent.TorrentFilesByName(files))
		c.JSON(http.StatusOK, gin.H{"files": files, "location": location, "tags": tags})
	} else {
		c.JSON(http.StatusInternalServerError, err)
	}
}

func main() {
	rand.Seed(time.Now().Unix())

	binPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	args := CommandLineArgs{}
	flag.UintVar(&args.Port, "port", 9999, "PORT to listen on")
	flag.StringVar(&args.Host, "host", "localhost", "HOST to bind to")
	flag.StringVar(&args.RPCSock, "rpc", "127.0.0.1:5000", "rtorrent scgi socket")
	flag.StringVar(&loginInfo.Username, "username", "admin", "Username used for logging in")
	flag.StringVar(&loginInfo.Password, "password", "", "Password used for logging in, omit for password-less login")
	cmdJwtKey := flag.String("jwt-key", "random", "JWT key used for signing the tokens")
	flag.Parse()

	// generate random key
	if len(loginInfo.Password) > 0 {
		if *cmdJwtKey == "random" {
			jwtSigningKey = RandKey(20)
			log.Println("JWT key generated...")
		} else {
			jwtSigningKey = []byte(*cmdJwtKey)
		}

		if loginInfo.Password == "random" {
			loginInfo.Password = string(RandKey(10))
		}
		log.Println("Username:", loginInfo.Username)
		log.Println("Passowrd:", loginInfo.Password)
	}

	var rtErr error
	rtNetwork := "tcp"
	if _, rtErr = os.Stat(args.RPCSock); rtErr == nil {
		rtNetwork = "unix"
	}
	rtConn, rtErr = rtorrent.Client(scgi.New(rtNetwork, args.RPCSock))
	if rtErr != nil {
		log.Fatalln(rtErr)
	}

	wwwPath := binPath + "/webroot/"
	router := gin.Default()
	router.StaticFS("/ui/", http.Dir(wwwPath))

	router.GET("/", Index)

	{
		router.POST("/login", Login)
		router.GET("/token", Token)
		router.GET("/api/list/:view", Protected(List))
		router.PUT("/api/add/new", Protected(AddNew))
		router.DELETE("/:hash", Protected(Delete))
		router.OPTIONS("/:hash/:action", Protected(DoAction))
		router.GET("/api/serverinfo", Protected(ServerInfo))
		router.GET("/details/:hash", Protected(Details))
	}

	hostToBind := fmt.Sprintf("%s:%d", args.Host, args.Port)

	log.Printf("serving at %s", hostToBind)
	serveErr := router.Run(hostToBind)
	if serveErr != nil {
		log.Fatalf("cannot start sever due to %s", serveErr)
	}
}
