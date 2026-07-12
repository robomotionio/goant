function check(a,b){if(a!==b)throw a+" !== "+b;} var c=false;try{decodeURIComponent('%');}catch(e){c=e instanceof URIError;}check(c,true);console.log('PASS');
