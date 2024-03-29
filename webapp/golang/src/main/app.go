package main

import (
	crand "crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/zenazn/goji"
	"github.com/zenazn/goji/web"
)

var (
	db             *sqlx.DB
	memcacheClient *memcache.Client
	store          *gsm.MemcacheStore

	userMtx    sync.Mutex
	postMtx    sync.Mutex
	commentMtx sync.Mutex

	indexTemplate       *template.Template
	postsTemplate       *template.Template
	accountNameTemplate *template.Template
)

const (
	postsPerPage         = 20
	PostsImageDir        = "/home/isucon/private_isu/webapp/public/image/"
	ISO8601_FORMAT       = "2006-01-02T15:04:05-07:00"
	UploadLimit    int64 = 10 * 1024 * 1024 // 10mb

	// CSRF Token error
	StatusUnprocessableEntity = 422
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func init() {
	memcacheClient = memcache.New("/tmp/memcached.sock")
	memcacheClient.Timeout = 300 * time.Millisecond
	memcacheClient.DeleteAll()
	store = gsm.NewMemcacheStore(memcacheClient, "isucogram_", []byte("sendagaya"))

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}
	indexTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	postsTemplate = template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
	accountNameTemplate = template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
}

func dbInitialize() {
	if files, err := filepath.Glob(PostsImageDir + "[1-9][0-9][0-9][0-9][0-9].*"); err == nil {
		for _, f := range files {
			slashIndex := strings.LastIndex(f, "/")
			dotIndex := strings.LastIndex(f, ".")
			if slashIndex < 0 {
				slashIndex = 0
			} else {
				slashIndex++
			}
			if dotIndex < 0 {
				continue
			}
			pid, err := strconv.Atoi(f[slashIndex:dotIndex])
			if err != nil {
				continue
			}
			if pid > 10000 {
				os.Remove(f)
			}
		}
	}

	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}

	resetCaches()
}

func resetCaches() {
	resetUserCache()
	resetCommentCache()
}

func tryLogin(accountName, password string) int {
	u := User{}
	err := db.Get(&u, "SELECT id, account_name, passhash FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return -1
	}

	if &u != nil && calculatePasshash(u.AccountName, password) == u.Passhash {
		return u.ID
	} else if &u == nil {
		return -1
	} else {
		return -1
	}
}

func validateUser(accountName, password string) bool {
	if !(regexp.MustCompile("\\A[0-9a-zA-Z_]{3,}\\z").MatchString(accountName) &&
		regexp.MustCompile("\\A[0-9a-zA-Z_]{6,}\\z").MatchString(password)) {
		return false
	}

	return true
}

// 今回のGo実装では言語側のエスケープの仕組みが使えないのでOSコマンドインジェクション対策できない
// 取り急ぎPHPのescapeshellarg関数を参考に自前で実装
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	out := sha512.Sum512([]byte(src))
	return fmt.Sprintf("%x", out)
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	value, ok := session.Values["user_id"]
	if !ok || value == nil {
		return User{}
	}
	uid := value.(int)
	users, err := getUsers([]int{uid})
	if err != nil {
		panic(err)
		return User{}
	}
	u, _ := users[uid]
	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func getUserCacheKey(uid int) string {
	return "user:" + strconv.Itoa(uid)
}

func getUsers(uids []int) (map[int]User, error) {
	users := make(map[int]User)

	keys := []string{}
	for _, uid := range uids {
		keys = append(keys, getUserCacheKey(uid))
	}

	userMtx.Lock()
	defer userMtx.Unlock()

	items, err := memcacheClient.GetMulti(keys)
	if items == nil && err != nil {
		fmt.Printf("error reading users from %s\n", err.Error())
	}

	missUids := []int{}
	for _, uid := range uids {
		key := getUserCacheKey(uid)
		item, ok := items[key]
		if ok {
			u := User{}
			err = json.Unmarshal(item.Value, &u)
			if err != nil {
				panic(fmt.Sprintf("error user unmarshal " + err.Error()))
			}
			users[uid] = u
		} else {
			missUids = append(missUids, uid)
		}
	}

	if len(missUids) > 0 {
		q, vs, err := sqlx.In("SELECT * FROM `users` WHERE `id` IN (?)", missUids)
		if err != nil {
			panic("sqlx.In " + err.Error())
		}
		missUsers := []User{}
		err = db.Select(&missUsers, q, vs...)
		for _, u := range missUsers {
			users[u.ID] = u
			key := getUserCacheKey(u.ID)
			userMarshaled, err := json.Marshal(&u)
			if err != nil {
				panic("userMarshaled: " + err.Error())
			}
			memcacheClient.Set(&memcache.Item{Key: key, Value: userMarshaled})
		}
	}

	return users, nil
}

func banUserOnCache(userID int) {
	u := User{}
	key := getUserCacheKey(userID)

	userMtx.Lock()
	defer userMtx.Unlock()

	item, err := memcacheClient.Get(key)
	if err != nil {
		return
	}
	err = json.Unmarshal(item.Value, &u)
	if err != nil {
		panic(fmt.Sprintf("error user unmarshal (ID: %d): %s\n", userID, err.Error()))
	}
	u.DelFlg = 1
	userMarshaled, err := json.Marshal(&u)
	if err != nil {
		panic(fmt.Sprintf("error user marshal (ID: %d): %s\n", userID, err.Error()))
		return
	}
	memcacheClient.Set(&memcache.Item{Key: key, Value: userMarshaled})
}

func appendUser(accountName string, passhash string) (int, error) {
	u := User{AccountName: accountName, Passhash: passhash, Authority: 0, DelFlg: 0, CreatedAt: time.Now()}
	userMtx.Lock()
	defer userMtx.Unlock()
	result, err := db.Exec("INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)", u.AccountName, u.Passhash)
	if err != nil {
		return -1, err
	}
	uid, err := result.LastInsertId()
	if err != nil {
		return -1, err
	}
	u.ID = int(uid)
	userMarshaled, err := json.Marshal(&u)
	if err != nil {
		return -1, err
	}
	memcacheClient.Set(&memcache.Item{Key: getUserCacheKey(u.ID), Value: userMarshaled})
	return u.ID, nil
}

func resetUserCache() {
	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users`")
	if err != nil {
		panic("error with SELECT * FROM `users`: " + err.Error())
	}
	for _, u := range users {
		key := getUserCacheKey(u.ID)
		userMarshaled, err := json.Marshal(&u)
		if err != nil {
			panic("userMarshaled: " + err.Error())
		}
		memcacheClient.Set(&memcache.Item{Key: key, Value: userMarshaled})
	}
}

func getIndexPostsCacheKey() string {
	return "indexPosts"
}

func getIndexPosts() ([]Post, error) {
	posts := []Post{}
	key := getIndexPostsCacheKey()
	postMtx.Lock()
	defer postMtx.Unlock()
	item, err := memcacheClient.Get(key)
	if err == nil {
		err = json.Unmarshal(item.Value, &posts)
		if err != nil {
			panic(fmt.Sprintf("error indexPosts unmarshal: %s\n", err.Error()))
		}
		return posts, nil
	}
	err = db.Select(&posts, "SELECT `posts`.`id`, `user_id`, `body`, `mime`, `posts`.`created_at` FROM `posts` WHERE `user_id` IN (SELECT `id` FROM `users` WHERE `del_flg` = 0) ORDER BY `created_at` DESC LIMIT ?", postsPerPage)
	if err != nil {
		return nil, err
	}
	postsMarshaled, err := json.Marshal(&posts)
	if err == nil {
		memcacheClient.Set(&memcache.Item{Key: key, Value: postsMarshaled})
	}
	return posts, nil
}

func getCommentsCacheKey(pid int) string {
	return "comments:" + strconv.Itoa(pid)
}

func getComments(pid int) ([]Comment, error) {
	comments := []Comment{}
	key := getCommentsCacheKey(pid)

	commentMtx.Lock()
	defer commentMtx.Unlock()
	item, err := memcacheClient.Get(key)
	if err == nil {
		err = json.Unmarshal(item.Value, &comments)
		if err != nil {
			panic(fmt.Sprintf("error comments unmarshal (ID: %d): %s\n", pid, err.Error()))
		}
		return comments, nil
	}
	// fmt.Printf("error reading comments (ID: %d) from %s\n", pid, err.Error())

	err = db.Select(&comments, "SELECT * FROM `comments` WHERE `post_id` = ? ORDER BY `created_at`", pid)
	if err != nil {
		return nil, err
	}

	commentsMarshaled, err := json.Marshal(&comments)
	if err == nil {
		memcacheClient.Set(&memcache.Item{Key: key, Value: commentsMarshaled})
	}

	return comments, nil
}

func appendComment(postID int, user *User, comment string) error {
	c := Comment{PostID: postID, UserID: user.ID, Comment: comment, CreatedAt: time.Now(), User: *user}
	key := getCommentsCacheKey(postID)
	comments := []Comment{}

	commentMtx.Lock()
	defer commentMtx.Unlock()
	result, err := db.Exec("INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)", c.PostID, c.UserID, c.Comment)
	if err != nil {
		return err
	}
	item, err := memcacheClient.Get(key)
	if err != nil {
		return nil
	}
	err = json.Unmarshal(item.Value, &comments)
	if err != nil {
		return err
	}
	cid, err := result.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = int(cid)
	comments = append(comments, c)
	commentsMarshaled, err := json.Marshal(&comments)
	if err != nil {
		return err
	}
	memcacheClient.Set(&memcache.Item{Key: key, Value: commentsMarshaled})
	return nil
}

func resetCommentCache() {
	postIDs := []int{}
	err := db.Select(&postIDs, "SELECT id FROM `posts`")
	if err != nil {
		panic("error with SELECT id FROM `posts`: " + err.Error())
	}
	for _, postID := range postIDs {
		getComments(postID)
	}
}

func makePosts(results []Post, CSRFToken string, allComments bool) ([]Post, error) {
	var posts []Post

	for _, p := range results {
		comments, err := getComments(p.ID)
		if err != nil {
			return nil, err
		}

		p.CommentCount = len(comments)
		if !allComments && p.CommentCount > 3 {
			comments = comments[:3]
		}

		uids := []int{p.UserID}
		for i := 0; i < len(comments); i++ {
			uids = append(uids, comments[i].UserID)
		}
		users, err := getUsers(uids)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(comments); i++ {
			comments[i].User, _ = users[comments[i].UserID]
		}

		p.Comments = comments
		p.User, _ = users[p.UserID]
		p.CSRFToken = CSRFToken

		if p.User.DelFlg == 0 {
			posts = append(posts, p)
		}
		if len(posts) >= postsPerPage {
			break
		}
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpeg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := io.ReadFull(crand.Reader, k); err != nil {
		panic("error reading from random source: " + err.Error())
	}
	return hex.EncodeToString(k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	userID := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if userID >= 0 {
		session := getSession(r)
		session.Values["user_id"] = userID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	session := getSession(r)
	uid, lerr := appendUser(accountName, calculatePasshash(accountName, password))
	if lerr != nil {
		fmt.Println("error: " + lerr.Error())
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	results, err := getIndexPosts()
	if err != nil {
		fmt.Println(err)
		return
	}

	posts, merr := makePosts(results, getCSRFToken(r), false)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	indexTemplate.Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(c web.C, w http.ResponseWriter, r *http.Request) {
	user := User{}
	uerr := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", c.URLParams["accountName"])

	if uerr != nil {
		fmt.Println(uerr)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	postMtx.Lock()
	rerr := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC LIMIT ?", user.ID, postsPerPage)
	postMtx.Unlock()
	if rerr != nil {
		fmt.Println(rerr)
		return
	}

	posts, merr := makePosts(results, getCSRFToken(r), false)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	commentCount := 0
	cerr := db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if cerr != nil {
		fmt.Println(cerr)
		return
	}

	postIDs := []int{}
	postMtx.Lock()
	perr := db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	postMtx.Unlock()
	if perr != nil {
		fmt.Println(perr)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []interface{}
		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		ccerr := db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if ccerr != nil {
			fmt.Println(ccerr)
			return
		}
	}

	me := getSessionUser(r)
	accountNameTemplate.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(parseErr)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, terr := time.Parse(ISO8601_FORMAT, maxCreatedAt)
	if terr != nil {
		fmt.Println(terr)
		return
	}

	results := []Post{}
	postMtx.Lock()
	rerr := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` IN (SELECT `id` FROM `users` WHERE `del_flg` = 0) AND `created_at` <= ? ORDER BY `created_at` DESC LIMIT ?", t.Format(ISO8601_FORMAT), postsPerPage)
	postMtx.Unlock()
	if rerr != nil {
		fmt.Println(rerr)
		return
	}

	posts, merr := makePosts(results, getCSRFToken(r), false)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	postsTemplate.Execute(w, posts)
}

func getPostsID(c web.C, w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.Atoi(c.URLParams["id"])
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	postMtx.Lock()
	rerr := db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	postMtx.Unlock()
	if rerr != nil {
		fmt.Println(rerr)
		return
	}

	posts, merr := makePosts(results, getCSRFToken(r), true)
	if merr != nil {
		fmt.Println(merr)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	file, header, ferr := r.FormFile("file")
	if ferr != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	ext := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
			ext = ".jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
			ext = ".png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
			ext = ".gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	fileSize, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		fmt.Println("error: " + err.Error())
		return
	}
	if fileSize > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	tempFile, err := ioutil.TempFile(PostsImageDir, "tmp-")
	if err != nil {
		fmt.Println("error: " + err.Error())
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		fmt.Println("error: " + err.Error())
		tempFile.Close()
		return
	}
	if _, err := io.Copy(tempFile, file); err != nil {
		fmt.Println("error: " + err.Error())
		tempFile.Close()
		return
	}
	tempFileName := tempFile.Name()
	tempFile.Close()

	postMtx.Lock()
	defer postMtx.Unlock()

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, eerr := db.Exec(
		query,
		me.ID,
		mime,
		"",
		r.FormValue("body"),
	)
	if eerr != nil {
		fmt.Println("error: " + eerr.Error())
		return
	}

	pid, lerr := result.LastInsertId()
	if lerr != nil {
		fmt.Println("error: " + lerr.Error())
		return
	}

	if err = os.Chmod(tempFileName, 0666); err != nil {
		fmt.Println("error: " + err.Error())
		return
	}
	if err = os.Rename(tempFileName, PostsImageDir+strconv.FormatInt(pid, 10)+ext); err != nil {
		fmt.Println("error: " + err.Error())
		return
	}

	memcacheClient.Delete(getIndexPostsCacheKey())

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
	return
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	postID, ierr := strconv.Atoi(r.FormValue("post_id"))
	if ierr != nil {
		fmt.Println("post_idは整数のみです")
		return
	}

	err := appendComment(postID, &me, r.FormValue("comment"))
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		fmt.Println(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	r.ParseForm()
	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
		uid, err := strconv.Atoi(id)
		if err != nil {
			go func() {
				banUserOnCache(uid)
			}()
		}
	}

	postMtx.Lock()
	memcacheClient.Delete(getIndexPostsCacheKey())
	postMtx.Unlock()

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	// go func() {
	// 	log.Println(http.ListenAndServe("localhost:6060", nil))
	// }()

	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	goji.Get("/initialize", getInitialize)
	goji.Get("/login", getLogin)
	goji.Post("/login", postLogin)
	goji.Get("/register", getRegister)
	goji.Post("/register", postRegister)
	goji.Get("/logout", getLogout)
	goji.Get("/", getIndex)
	goji.Get(regexp.MustCompile(`^/@(?P<accountName>[a-zA-Z]+)$`), getAccountName)
	goji.Get("/posts", getPosts)
	goji.Get("/posts/:id", getPostsID)
	goji.Post("/", postIndex)
	goji.Post("/comment", postComment)
	goji.Get("/admin/banned", getAdminBanned)
	goji.Post("/admin/banned", postAdminBanned)
	goji.Get("/*", http.FileServer(http.Dir("../../../public")))
	goji.Serve()
}
