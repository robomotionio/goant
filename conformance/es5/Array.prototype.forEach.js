function check(a,b){if(a!==b)throw a+" !== "+b;} var r=[];[1,2,3].forEach(function(x){r.push(x*2);});check(r.join(','),'2,4,6');console.log('PASS');
